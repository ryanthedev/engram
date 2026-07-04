//go:build e2e

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

var harness *Harness

// TestMain boots the local stack once for the whole e2e suite.
func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	h, err := Boot(ctx)
	if err != nil {
		panic("e2e boot failed: " + err.Error())
	}
	harness = h
	code := m.Run()
	h.Shutdown()
	os.Exit(code)
}

// TestDW_3_1_FullLoopThroughMCP: the headline loop — MCP ingest → worker
// extract/reconcile (stub LLM) → MCP search returns the fact — runs green
// against the booted stack.
func TestDW_3_1_FullLoopThroughMCP(t *testing.T) {
	tenant := uniqueTenant("dw31")
	raw, _, err := harness.MintToken(tenant, "alice", "claude", time.Hour)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	conn, err := harness.MCP(raw)
	if err != nil {
		t.Fatalf("spawn mcp: %v", err)
	}
	defer conn.Close()

	init, err := conn.Initialize()
	if err != nil || init.Result["protocolVersion"] == nil {
		t.Fatalf("initialize failed: %v (%v)", err, init)
	}
	tools, err := conn.ListTools()
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if !contains(tools, "memory_ingest") || !contains(tools, "memory_search") || !contains(tools, "memory_status") {
		t.Fatalf("tools = %v, want the three memory tools", tools)
	}

	sc, err := conn.CallTool("memory_ingest", map[string]any{
		"event_id": tenant + "-e1",
		"text":     "fact: engram | retrieval | hybrid-rrf",
	})
	if err != nil {
		t.Fatalf("memory_ingest: %v", err)
	}
	if sc["id"] == nil {
		t.Fatalf("ingest returned no id: %v", sc)
	}
	if err := waitForHit(conn, "engram retrieval hybrid-rrf", "hybrid-rrf"); err != nil {
		t.Fatalf("full loop: %v", err)
	}
}

// TestDW_3_2_CLICommands: token create/list/revoke, ingest, search, status all
// work against the local stack.
func TestDW_3_2_CLICommands(t *testing.T) {
	tenant := uniqueTenant("dw32")
	raw, handle, err := harness.MintToken(tenant, "carol", "cli", time.Hour)
	if err != nil {
		t.Fatalf("token create: %v", err)
	}

	// list shows the handle.
	list, err := harness.RunCLI(nil, "token", "list", "--tenant", tenant, "--user", "carol")
	if err != nil {
		t.Fatalf("token list: %v (%s)", err, list)
	}
	if !strings.Contains(list, handle) {
		t.Fatalf("token list missing handle %s:\n%s", handle, list)
	}

	env := map[string]string{"ENGRAM_TOKEN": raw}
	// ingest.
	ing, err := harness.RunCLI(env, "ingest", "--event-id", tenant+"-c1", "--text", "fact: carol | uses | the-cli")
	if err != nil {
		t.Fatalf("ingest: %v (%s)", err, ing)
	}
	// status reports the resolved identity + healthy.
	st, err := harness.RunCLI(env, "status")
	if err != nil {
		t.Fatalf("status: %v (%s)", err, st)
	}
	if !strings.Contains(st, tenant) || !strings.Contains(st, "\"healthy\": true") {
		t.Fatalf("status did not report healthy identity:\n%s", st)
	}
	// search eventually finds the fact.
	deadline := time.Now().Add(20 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		out, err := harness.RunCLI(env, "search", "carol uses the-cli", "-k", "10")
		if err == nil && strings.Contains(out, "the-cli") {
			found = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !found {
		t.Fatal("CLI search never returned the ingested fact")
	}

	// revoke, then an authenticated call is rejected.
	if out, err := harness.RunCLI(nil, "token", "revoke", handle); err != nil {
		t.Fatalf("token revoke: %v (%s)", err, out)
	}
	if _, err := harness.RunCLI(env, "status"); err == nil {
		t.Fatal("status succeeded with a revoked token")
	}
}

// TestDW_3_3_UnauthAndRevocation: a call with no token is rejected, and a
// revoked token stops working on the next call (well within 5 s — revocation
// is query-time).
func TestDW_3_3_UnauthAndRevocation(t *testing.T) {
	tenant := uniqueTenant("dw33")
	raw, handle, err := harness.MintToken(tenant, "dave", "cli", time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// No token: rejected.
	if _, err := harness.RunCLI(map[string]string{"ENGRAM_TOKEN": ""}, "status"); err == nil {
		t.Fatal("status without a token succeeded")
	}

	env := map[string]string{"ENGRAM_TOKEN": raw}
	if _, err := harness.RunCLI(env, "status"); err != nil {
		t.Fatalf("status with a valid token failed: %v", err)
	}

	start := time.Now()
	if out, err := harness.RunCLI(nil, "token", "revoke", handle); err != nil {
		t.Fatalf("revoke: %v (%s)", err, out)
	}
	if _, err := harness.RunCLI(env, "status"); err == nil {
		t.Fatal("status succeeded after revocation")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("revocation took %v to bite, want <=5s", elapsed)
	}
}

// TestDW_3_6_ScenarioPackRunsWithoutCoreEdits: every registered scenario runs
// green, and the sample pack is present — proving the zero-core-edit extension
// point (the sample pack file registered via init, discovered by the core).
func TestDW_3_6_ScenarioPackRunsWithoutCoreEdits(t *testing.T) {
	scenarios := Scenarios()
	if len(scenarios) < 2 {
		t.Fatalf("registered scenarios = %d, want the sample pack (>=2)", len(scenarios))
	}
	var names []string
	for _, s := range scenarios {
		names = append(names, s.Name)
	}
	if !contains(names, "sample/mcp-round-trip") {
		t.Fatalf("sample pack not registered: %v", names)
	}
	for _, s := range scenarios {
		s := s
		t.Run(s.Name, func(t *testing.T) {
			if err := s.Run(harness); err != nil {
				t.Fatalf("scenario %s failed: %v", s.Name, err)
			}
		})
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
