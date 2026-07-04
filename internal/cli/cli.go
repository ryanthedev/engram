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
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ryanthedev/engram/internal/auth"
	"github.com/ryanthedev/engram/internal/engramclient"
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
	case "ingest":
		err = runIngest(ctx, rest, env, out)
	case "search":
		err = runSearch(ctx, rest, env, out)
	case "status":
		err = runStatus(ctx, rest, env, out)
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
  engram token create   --tenant T --user U [--agent A] [--ttl 720h] [--url URL]
  engram token list     --tenant T --user U [--url URL]
  engram token revoke   <handle> [--url URL]
  engram ingest         --event-id ID --text TEXT [--source S] [-addr HOST:PORT] [-token TOK]
  engram search         QUERY [-k 10] [-addr HOST:PORT] [-token TOK]
  engram status         [-addr HOST:PORT] [-token TOK]

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
	ttl := fs.Duration("ttl", 720*time.Hour, "token lifetime")
	url := fs.String("url", "", "OpenSearch URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id := auth.Identity{TenantID: *tenant, UserID: *user, AgentID: *agent}
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

func runIngest(ctx context.Context, args []string, env Env, out io.Writer) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	eventID := fs.String("event-id", "", "idempotency event id (required)")
	text := fs.String("text", "", "event text (required)")
	source := fs.String("source", "", "source id")
	addr := fs.String("addr", "", "engramd address")
	token := fs.String("token", "", "bearer token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *eventID == "" || *text == "" {
		return errors.New("ingest: --event-id and --text are required")
	}
	client, err := dialClient(env, *addr, *token)
	if err != nil {
		return err
	}
	defer client.Close()
	id, err := client.Ingest(ctx, *eventID, *text, *source)
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
	hits, err := client.Search(ctx, query, *k)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Fprintln(out, "no hits")
		return nil
	}
	for _, h := range hits {
		fmt.Fprintf(out, "%.4f  %-10s  %s\n", h.Score, h.Source, h.ID)
		if h.Fields != "" {
			fmt.Fprintf(out, "        %s\n", h.Fields)
		}
	}
	return nil
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
