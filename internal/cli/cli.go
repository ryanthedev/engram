// Package cli implements the `engram` command: token administration plus the
// ingest/search/status client surface over engramd's gRPC API. It is written
// as a Run function over explicit streams and an env lookup so it is testable
// without a real process (the e2e harness and unit tests both drive Run).
//
// Command groups:
//   - token create/list/revoke — admin operations against the token store
//     (OpenSearch directly; issuing a token cannot itself require a token).
//   - ingest / search / status — authenticated gRPC calls carrying a bearer
//     token (-token or ENGRAM_TOKEN).
//   - knowledge collections/create-collection — knowledge-collection registry
//     administration, also over gRPC with a bearer token (create requires the
//     admin role). A collection must exist before any harvest targets it.
//   - purge — the memory tier's only delete path (requires the memory-admin
//     role). Dry-run by default; see purge.go for why.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ryanthedev/engram/internal/acl"
	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/engramclient"
	"github.com/ryanthedev/engram/internal/experience"
	"github.com/ryanthedev/engram/internal/mcp"
	"github.com/ryanthedev/engram/internal/store"
)

// Env resolves an environment variable (injected for testability).
type Env func(string) string

// Run executes one `engram` invocation and returns a process exit code. args
// excludes the program name. All output goes to out; errors and usage to errW.
func Run(ctx context.Context, args []string, env Env, out, errW io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errW, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "token":
		err = runToken(ctx, rest, env, out, errW)
	case "acl":
		err = runACL(ctx, rest, env, out)
	case "quarantine":
		err = runQuarantine(ctx, rest, env, out)
	case "ingest":
		err = runIngest(ctx, rest, env, out, errW)
	case "search":
		err = runSearch(ctx, rest, env, out)
	case "status":
		err = runStatus(ctx, rest, env, out)
	case "audit":
		err = runAudit(ctx, rest, env, out)
	case "export":
		err = runExport(ctx, rest, env, out)
	case "knowledge":
		err = runKnowledge(ctx, rest, env, out)
	case "purge":
		err = runPurge(ctx, rest, env, out)
	case "help", "-h", "--help":
		fmt.Fprintln(out, usage)
		return 0
	default:
		fmt.Fprintf(errW, "engram: unknown command %q\n\n%s\n", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(errW, "engram:", err)
		return 1
	}
	return 0
}

const usage = `engram — Engram memory CLI

Usage:
  engram token create   --tenant T --user U [--agent A] [--roles R1,R2] [--ttl 720h] [--url URL]
  engram token list     --tenant T --user U [--url URL]
  engram token revoke   <handle> [--url URL]
  engram acl grant      --tenant T --user U (--agent A | --team M | --org) [--url URL]
  engram acl revoke     --tenant T --user U (--agent A | --team M | --org) [--url URL]
  engram acl list       --tenant T --user U [--url URL]
  engram ingest         --event-id ID --text TEXT [--source S] [--scope private|team|org] [--team M] [-addr HOST:PORT] [-token TOK]
  engram search         QUERY [-k 10] [-addr HOST:PORT] [-token TOK]
  engram status         [-addr HOST:PORT] [-token TOK]
  engram audit          <fact-id> [-addr HOST:PORT] [-token TOK]
  engram export         <dir> [--force] [-addr HOST:PORT] [-token TOK]
  engram purge          --event-id ID [--event-id ID2 ...] [--confirm] [-addr HOST:PORT] [-token TOK]
  engram quarantine list    --tenant T [--url URL]
  engram quarantine release <fingerprint> --tenant T [--url URL]
  engram knowledge collections [-addr HOST:PORT] [-token TOK]
  engram knowledge create-collection --name N [--text-field F] [--public] [--roles R1,R2]
                                     [--field NAME:TYPE[:filterable][:sortable]]... [-addr HOST:PORT] [-token TOK]

Ingest text is normally plain prose — the production extractor is an LLM that
reads prose directly, so plain prose needs no special formatting. --text may
also carry optional pipe-delimited directive lines (one per line), mainly
useful for deterministic fixtures/local runs without an LLM:
  fact: subject | predicate | object [@ RFC3339 valid_at]
  retract: subject | predicate [@ RFC3339 valid_at]
  experience: task | distilled skill | success|failure | phi=0..1 | evidence=e1;e2 | signals=s1;s2 | context=...
A line starting with fact:/retract:/experience: that fails to parse this
grammar prints a non-fatal advisory to stderr; it never blocks the ingest.

--event-id is not a full idempotency key: derived semantic facts are deduped by content;
the raw episodic log entry for --text, however, is appended on every ingest call — replaying
the same --event-id does not deduplicate the raw log.

purge is the only way to remove ingested memory, and it is DRY RUN BY DEFAULT: without
--confirm it just reports what would go. It hard-deletes the episodic and extraction-ledger
rows for each --event-id (every replayed duplicate included) and soft-deletes the semantic
facts extracted from them (expired_at stamped: gone from search, still visible to audit).
The graph tier is untouched — rebuild it with cmd/engram-graph-rebuild. The token must
carry the memory-admin role, and the tenant comes from the token, never from a flag.

Environment: ENGRAM_OPENSEARCH_URL, ENGRAM_ADDR, ENGRAM_TOKEN.`

// --- token admin (OpenSearch-backed) ---

func runToken(ctx context.Context, args []string, env Env, out, errW io.Writer) error {
	if len(args) == 0 {
		return errors.New("token: expected create|list|revoke")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return runTokenCreate(ctx, rest, env, out)
	case "list":
		return runTokenList(ctx, rest, env, out)
	case "revoke":
		return runTokenRevoke(ctx, rest, env, out)
	default:
		return fmt.Errorf("token: unknown subcommand %q", sub)
	}
}

func tokenIssuer(env Env, url string) *auth.TokenIssuer {
	base := firstNonEmpty(url, env("ENGRAM_OPENSEARCH_URL"), "http://localhost:9200")
	ts := store.NewAuthTokenStore(&http.Client{Timeout: store.DefaultTimeout}, base)
	return auth.NewTokenIssuer(ts, nil)
}

func runTokenCreate(ctx context.Context, args []string, env Env, out io.Writer) error {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tenant := fs.String("tenant", "", "tenant id (required)")
	user := fs.String("user", "", "user id (required)")
	agent := fs.String("agent", "", "agent id")
	roles := fs.String("roles", "", "comma-separated role list (e.g. admin,harvester)")
	ttl := fs.Duration("ttl", 720*time.Hour, "token lifetime")
	url := fs.String("url", "", "OpenSearch URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id := auth.Identity{TenantID: *tenant, UserID: *user, AgentID: *agent, Roles: parseRoles(*roles)}
	if !id.Valid() {
		return errors.New("token create: --tenant and --user are required")
	}
	raw, handle, err := tokenIssuer(env, *url).Issue(ctx, id, *ttl)
	if err != nil {
		return err
	}
	// The raw token is shown exactly once — make that explicit to the operator.
	fmt.Fprintf(out, "token created (handle %s) — copy the raw token now, it is not recoverable:\n%s\n", handle, raw)
	return nil
}

// parseRoles turns the raw --roles flag value into a role slice for
// auth.Identity. An empty or whitespace-only value (including the flag's
// zero-value default, i.e. omitted) means "no roles" and returns nil, never
// a single-empty-string slice — matching today's role-less behavior exactly
// (DW-1.2). Per-entry cleanup (trimming, dropping empty segments, deduping)
// is deliberately NOT duplicated here: auth.TokenIssuer.Issue always
// re-normalizes id.Roles via normalizeRoles before persisting (the mint-time
// half of the claim barricade), so doing it again here would just be the
// same sanitization rule implemented twice with room to drift.
func parseRoles(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

func runTokenList(ctx context.Context, args []string, env Env, out io.Writer) error {
	fs := flag.NewFlagSet("token list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tenant := fs.String("tenant", "", "tenant id (required)")
	user := fs.String("user", "", "user id (required)")
	url := fs.String("url", "", "OpenSearch URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id := auth.Identity{TenantID: *tenant, UserID: *user}
	if !id.Valid() {
		return errors.New("token list: --tenant and --user are required")
	}
	recs, err := tokenIssuer(env, *url).List(ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%-16s  %-10s  %-20s  %s\n", "HANDLE", "AGENT", "EXPIRES", "STATE")
	for _, r := range recs {
		state := "active"
		if r.Revoked {
			state = "revoked"
		} else if !r.ExpiresAt.IsZero() && time.Now().After(r.ExpiresAt) {
			state = "expired"
		}
		expires := "never"
		if !r.ExpiresAt.IsZero() {
			expires = r.ExpiresAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(out, "%-16s  %-10s  %-20s  %s\n", r.ID, r.AgentID, expires, state)
	}
	return nil
}

func runTokenRevoke(ctx context.Context, args []string, env Env, out io.Writer) error {
	fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	url := fs.String("url", "", "OpenSearch URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("token revoke: expected exactly one <handle>")
	}
	handle := fs.Arg(0)
	if err := tokenIssuer(env, *url).Revoke(ctx, handle); err != nil {
		return err
	}
	fmt.Fprintf(out, "token %s revoked\n", handle)
	return nil
}

// --- authenticated gRPC surface ---

func dialClient(env Env, addr, token string) (*engramclient.Client, error) {
	a := firstNonEmpty(addr, env("ENGRAM_ADDR"), "localhost:7070")
	tok := firstNonEmpty(token, env("ENGRAM_TOKEN"))
	return engramclient.Dial(a, tok)
}

func runIngest(ctx context.Context, args []string, env Env, out, errW io.Writer) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	eventID := fs.String("event-id", "",
		"event id (required); derived semantic facts are deduped by content, not by this id — "+
			"the raw episodic log entry is appended on every call regardless")
	text := fs.String("text", "", "event text (required)")
	source := fs.String("source", "", "source id")
	scope := fs.String("scope", "", "scope: private|team|org (default private)")
	team := fs.String("team", "", "team id (required for --scope team)")
	addr := fs.String("addr", "", "engramd address")
	token := fs.String("token", "", "bearer token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *eventID == "" || *text == "" {
		return errors.New("ingest: --event-id and --text are required")
	}
	// Client-side advisory only: never blocks or changes the ingest outcome,
	// just flags a directive-looking line that won't extract as intended.
	if msg := directiveAdvisory(*text); msg != "" {
		fmt.Fprintln(errW, msg)
	}
	client, err := dialClient(env, *addr, *token)
	if err != nil {
		return err
	}
	defer client.Close()
	id, err := client.IngestScoped(ctx, *eventID, *text, *source, *scope, *team)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "ingested %s -> %s\n", *eventID, id)
	return nil
}

func runSearch(ctx context.Context, args []string, env Env, out io.Writer) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	k := fs.Int("k", 10, "max hits")
	addr := fs.String("addr", "", "engramd address")
	token := fs.String("token", "", "bearer token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("search: expected a query")
	}
	query := fs.Arg(0)
	client, err := dialClient(env, *addr, *token)
	if err != nil {
		return err
	}
	defer client.Close()
	// The CLI is an unfiltered search: a zero SearchFilter is exactly the query
	// it issued before filters existed.
	res, err := client.Search(ctx, query, *k, mcp.SearchFilter{})
	if err != nil {
		return err
	}
	if len(res.Hits) == 0 && len(res.Expanded) == 0 {
		fmt.Fprintln(out, "no hits")
		return nil
	}
	printHits(out, res.Hits)
	// Graph expansions print below their own header, never mixed into the
	// matched hits: they did not match the query, and -k did not ask for them.
	if len(res.Expanded) > 0 {
		fmt.Fprintf(out, "\nexpanded (%d graph hit(s) reached from the matches above; not counted against -k)\n", len(res.Expanded))
		printHits(out, res.Expanded)
	}
	return nil
}

// printHits writes one block of search hits, one hit per line plus its raw
// fields.
func printHits(out io.Writer, hits []mcp.Hit) {
	for _, h := range hits {
		fmt.Fprintf(out, "%.4f  %-10s  %s\n", h.Score, h.Source, h.ID)
		if h.Fields != "" {
			fmt.Fprintf(out, "        %s\n", h.Fields)
		}
	}
}

func runStatus(ctx context.Context, args []string, env Env, out io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", "", "engramd address")
	token := fs.String("token", "", "bearer token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client, err := dialClient(env, *addr, *token)
	if err != nil {
		return err
	}
	defer client.Close()
	st, err := client.Status(ctx)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	fmt.Fprintln(out, string(b))
	return nil
}

func runAudit(ctx context.Context, args []string, env Env, out io.Writer) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", "", "engramd address")
	token := fs.String("token", "", "bearer token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("audit: expected exactly one <fact-id>")
	}
	client, err := dialClient(env, *addr, *token)
	if err != nil {
		return err
	}
	defer client.Close()
	res, err := client.Audit(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Fprintln(out, string(b))
	return nil
}

// --- ACL edge admin (OpenSearch-backed, like token admin) ---

func aclEdgeStore(env Env, url string) *store.ACLEdgeStore {
	base := firstNonEmpty(url, env("ENGRAM_OPENSEARCH_URL"), "http://localhost:9200")
	return store.NewACLEdgeStore(&http.Client{Timeout: store.DefaultTimeout}, base)
}

func runACL(ctx context.Context, args []string, env Env, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("acl: expected grant|revoke|list")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "grant":
		return runACLEdge(ctx, rest, env, out, true)
	case "revoke":
		return runACLEdge(ctx, rest, env, out, false)
	case "list":
		return runACLList(ctx, rest, env, out)
	default:
		return fmt.Errorf("acl: unknown subcommand %q", sub)
	}
}

// aclEdgeFromFlags parses the shared edge flags into an acl.Edge, inferring the
// edge type from which target flag is set (exactly one of --agent/--team/--org).
func aclEdgeFromFlags(args []string) (acl.Edge, string, error) {
	fs := flag.NewFlagSet("acl edge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tenant := fs.String("tenant", "", "tenant id (required)")
	user := fs.String("user", "", "user id (required)")
	agent := fs.String("agent", "", "agent id (user_agent edge)")
	team := fs.String("team", "", "team id (member edge)")
	org := fs.Bool("org", false, "org write grant (org_grant edge)")
	url := fs.String("url", "", "OpenSearch URL")
	if err := fs.Parse(args); err != nil {
		return acl.Edge{}, "", err
	}
	if *tenant == "" || *user == "" {
		return acl.Edge{}, "", errors.New("acl: --tenant and --user are required")
	}
	set := 0
	e := acl.Edge{TenantID: *tenant, UserID: *user}
	if *agent != "" {
		e.Type, e.AgentID = acl.EdgeUserAgent, *agent
		set++
	}
	if *team != "" {
		e.Type, e.TeamID = acl.EdgeMember, *team
		set++
	}
	if *org {
		e.Type = acl.EdgeOrgGrant
		set++
	}
	if set != 1 {
		return acl.Edge{}, "", errors.New("acl: exactly one of --agent, --team, or --org is required")
	}
	return e, *url, nil
}

func runACLEdge(ctx context.Context, args []string, env Env, out io.Writer, grant bool) error {
	e, url, err := aclEdgeFromFlags(args)
	if err != nil {
		return err
	}
	es := aclEdgeStore(env, url)
	if grant {
		if err := es.PutEdge(ctx, e); err != nil {
			return err
		}
		fmt.Fprintf(out, "granted %s edge (user=%s agent=%s team=%s)\n", e.Type, e.UserID, e.AgentID, e.TeamID)
		return nil
	}
	found, err := es.DeleteEdge(ctx, e)
	if err != nil {
		return err
	}
	if !found {
		fmt.Fprintf(out, "no such %s edge (nothing to revoke)\n", e.Type)
		return nil
	}
	fmt.Fprintf(out, "revoked %s edge (user=%s agent=%s team=%s)\n", e.Type, e.UserID, e.AgentID, e.TeamID)
	return nil
}

func runACLList(ctx context.Context, args []string, env Env, out io.Writer) error {
	fs := flag.NewFlagSet("acl list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tenant := fs.String("tenant", "", "tenant id (required)")
	user := fs.String("user", "", "user id (required)")
	url := fs.String("url", "", "OpenSearch URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id := auth.Identity{TenantID: *tenant, UserID: *user}
	if !id.Valid() {
		return errors.New("acl list: --tenant and --user are required")
	}
	edges, err := aclEdgeStore(env, *url).ListEdges(ctx, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%-12s  %-12s  %-12s\n", "TYPE", "AGENT", "TEAM")
	for _, e := range edges {
		fmt.Fprintf(out, "%-12s  %-12s  %-12s\n", e.Type, e.AgentID, e.TeamID)
	}
	return nil
}

// --- quarantine admin (OpenSearch-backed, like acl/token admin) ---

// experienceStore builds an OpenSearch-backed Store for the admin
// surface. Release admits directly (a human reviewed it) and never invokes the
// gate, so the mandatory rule gate here is only to satisfy construction — it is
// never consulted on this path.
func experienceStore(env Env, url string) (*experience.Store, error) {
	base := firstNonEmpty(url, env("ENGRAM_OPENSEARCH_URL"), "http://localhost:9200")
	backend := experience.NewOpenSearchBackend(&http.Client{Timeout: store.DefaultTimeout}, base)
	return experience.NewStore(experience.RuleGatekeeper{}, backend, nil)
}

func runQuarantine(ctx context.Context, args []string, env Env, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("quarantine: expected list|release")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return runQuarantineList(ctx, rest, env, out)
	case "release":
		return runQuarantineRelease(ctx, rest, env, out)
	default:
		return fmt.Errorf("quarantine: unknown subcommand %q", sub)
	}
}

func runQuarantineList(ctx context.Context, args []string, env Env, out io.Writer) error {
	fs := flag.NewFlagSet("quarantine list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tenant := fs.String("tenant", "", "tenant id (required)")
	url := fs.String("url", "", "OpenSearch URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tenant == "" {
		return errors.New("quarantine list: --tenant is required")
	}
	es, err := experienceStore(env, *url)
	if err != nil {
		return err
	}
	items, err := es.ListQuarantine(ctx, *tenant)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Fprintln(out, "no quarantined experiences")
		return nil
	}
	fmt.Fprintf(out, "%-64s  %-10s  %s\n", "FINGERPRINT", "VERDICT", "REASON")
	for _, q := range items {
		fmt.Fprintf(out, "%-64s  %-10s  %s\n", q.Experience.Fingerprint, q.Verdict, q.Reason)
	}
	return nil
}

func runQuarantineRelease(ctx context.Context, args []string, env Env, out io.Writer) error {
	fs := flag.NewFlagSet("quarantine release", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tenant := fs.String("tenant", "", "tenant id (required)")
	url := fs.String("url", "", "OpenSearch URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("quarantine release: expected exactly one <fingerprint>")
	}
	if *tenant == "" {
		return errors.New("quarantine release: --tenant is required")
	}
	es, err := experienceStore(env, *url)
	if err != nil {
		return err
	}
	if err := es.Release(ctx, *tenant, fs.Arg(0)); err != nil {
		return err
	}
	fmt.Fprintf(out, "released %s from quarantine into the admitted tier\n", fs.Arg(0))
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
