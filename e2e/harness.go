//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Harness is a booted local Engram stack: a deterministic embedding server, a
// stub extraction LLM, and engramd (with the outbox worker + repair sweep +
// auth barricade), all wired together over real HTTP/gRPC. It drives the stack
// through the CLI, the MCP server, and direct gRPC. Boot self-hosts the Go
// components against a running OpenSearch (ENGRAM_OPENSEARCH_URL, default
// localhost:9200); the compose stack (`make e2e`) is the same topology in
// containers. Per-run tenant isolation means scenarios never collide on a
// shared cluster.
type Harness struct {
	OSURL    string
	GRPCAddr string

	engramBin string
	mcpBin    string

	procs []*exec.Cmd
	root  string
}

// Boot returns a ready Harness. When ENGRAM_E2E_ADDR is set, it targets an
// already-running stack (the compose stack that `make e2e` boots) and only
// builds the CLI/MCP client binaries. Otherwise it self-hosts embed + stub +
// engramd against a running OpenSearch (ENGRAM_OPENSEARCH_URL, default
// localhost:9200) — the mode used for fast local iteration. The caller must
// Shutdown it. Boot fails fast if the stack is unreachable.
func Boot(ctx context.Context) (*Harness, error) {
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	osURL := envOr("ENGRAM_OPENSEARCH_URL", "http://localhost:9200")

	h := &Harness{OSURL: osURL, root: root}

	// The CLI and MCP surfaces are always built and run on the host (they are
	// the client edges the harness drives).
	for _, cmd := range []string{"engram", "engram-mcp"} {
		out := filepath.Join(root, ".e2e-bin", cmd)
		if err := build(ctx, root, cmd, out); err != nil {
			return nil, err
		}
		if cmd == "engram" {
			h.engramBin = out
		} else {
			h.mcpBin = out
		}
	}

	// External stack (compose): connect and return.
	if addr := os.Getenv("ENGRAM_E2E_ADDR"); addr != "" {
		h.GRPCAddr = addr
		if err := waitTCP(ctx, addr); err != nil {
			return nil, fmt.Errorf("external engramd at %s not reachable: %w", addr, err)
		}
		return h, nil
	}

	// Self-boot: build and start the stack services on the host.
	bins := map[string]string{}
	for _, cmd := range []string{"engram-embed-server", "engram-stub-llm", "engram-server"} {
		out := filepath.Join(root, ".e2e-bin", cmd)
		if err := build(ctx, root, cmd, out); err != nil {
			return nil, err
		}
		bins[cmd] = out
	}

	embedPort, stubPort, grpcPort := freePort(), freePort(), freePort()
	h.GRPCAddr = fmt.Sprintf("localhost:%d", grpcPort)

	// Embedding server (TEI contract).
	if err := h.start(ctx, bins["engram-embed-server"], nil,
		"-addr", fmt.Sprintf(":%d", embedPort)); err != nil {
		return nil, err
	}
	// Stub extraction LLM (OpenAI-compatible).
	if err := h.start(ctx, bins["engram-stub-llm"], nil,
		"-addr", fmt.Sprintf(":%d", stubPort)); err != nil {
		return nil, err
	}
	if err := waitHTTP(ctx, fmt.Sprintf("http://localhost:%d/health", embedPort)); err != nil {
		h.Shutdown()
		return nil, fmt.Errorf("embed server not healthy: %w", err)
	}
	if err := waitHTTP(ctx, fmt.Sprintf("http://localhost:%d/health", stubPort)); err != nil {
		h.Shutdown()
		return nil, fmt.Errorf("stub LLM not healthy: %w", err)
	}

	// engramd wired to both real-HTTP services, with fast e2e timings.
	if err := h.start(ctx, bins["engram-server"], nil,
		"-addr", h.GRPCAddr,
		"-url", osURL,
		"-embed-url", fmt.Sprintf("http://localhost:%d", embedPort),
		"-extract-url", fmt.Sprintf("http://localhost:%d", stubPort),
		"-poll-interval", "200ms",
		"-sweep-interval", "1s",
		"-enrich-interval", "200ms",
	); err != nil {
		h.Shutdown()
		return nil, err
	}
	if err := waitTCP(ctx, h.GRPCAddr); err != nil {
		h.Shutdown()
		return nil, fmt.Errorf("engramd gRPC not listening: %w", err)
	}
	return h, nil
}

// Shutdown stops every started process.
func (h *Harness) Shutdown() {
	for i := len(h.procs) - 1; i >= 0; i-- {
		p := h.procs[i]
		if p.Process != nil {
			_ = p.Process.Kill()
			_ = p.Wait()
		}
	}
}

// MintToken issues a token via the CLI (`engram token create`) and returns the
// raw token and its handle. This is the real admin surface (DW-3.2).
func (h *Harness) MintToken(tenant, user, agent string, ttl time.Duration) (raw, handle string, err error) {
	out, err := h.RunCLI(nil, "token", "create",
		"--tenant", tenant, "--user", user, "--agent", agent, "--ttl", ttl.String())
	if err != nil {
		return "", "", fmt.Errorf("token create: %w (%s)", err, out)
	}
	raw = tokenRe.FindString(out)
	handle = handleRe.FindString(out)
	if raw == "" {
		return "", "", fmt.Errorf("could not parse raw token from output: %s", out)
	}
	return raw, strings.TrimPrefix(handle, "handle "), nil
}

var (
	tokenRe  = regexp.MustCompile(`egm_[A-Za-z0-9_-]{43}`)
	handleRe = regexp.MustCompile(`handle [0-9a-f]{16}`)
)

// RunCLI runs the engram CLI with the given args and env overrides (added to
// the base OpenSearch/addr env), returning combined output.
func (h *Harness) RunCLI(extraEnv map[string]string, args ...string) (string, error) {
	cmd := exec.Command(h.engramBin, args...)
	cmd.Env = append(os.Environ(),
		"ENGRAM_OPENSEARCH_URL="+h.OSURL,
		"ENGRAM_ADDR="+h.GRPCAddr,
	)
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// build compiles ./cmd/<name> to out.
func build(ctx context.Context, root, name, out string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/"+name)
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("building %s: %w: %s", name, err, b)
	}
	return nil
}

// start launches a background process and records it for Shutdown.
func (h *Harness) start(ctx context.Context, bin string, extraEnv []string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = h.root
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdout = os.Stderr // stack logs to test stderr (never stdout)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", filepath.Base(bin), err)
	}
	h.procs = append(h.procs, cmd)
	return nil
}

// MCP spawns an engram-mcp subprocess authenticated with token and returns a
// JSON-RPC connection to it over stdio (the exact transport a real agent host
// uses). Close it via (*MCPConn).Close.
func (h *Harness) MCP(token string) (*MCPConn, error) {
	cmd := exec.Command(h.mcpBin, "-addr", h.GRPCAddr, "-token", token)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &MCPConn{cmd: cmd, in: stdin, out: bufio.NewReader(stdout)}, nil
}

// --- small helpers ---

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func freePort() int {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		panic(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitTCP(ctx context.Context, addr string) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", addr)
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// discard keeps json happy for helpers that ignore results.
var _ = json.Marshal
