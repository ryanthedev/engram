package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// --- memory purge (gRPC-backed) ---
//
// `engram purge` is the operator escape hatch from append-only memory ingest.
// Ingest is otherwise permanent, and --event-id does NOT deduplicate the raw
// episodic log (a replayed ingest appends a SECOND doc under the same event
// id — D13's idempotency lives on the extraction ledger, not on the append),
// so a botched bulk migration has no undo without this verb.
//
// Two guardrails are deliberate, and both live here at the operator's edge
// rather than only on the server:
//
//   - DRY RUN BY DEFAULT. Running `engram purge --event-id x` reports what
//     WOULD be removed and mutates nothing. --confirm is what actually erases.
//     The inverse default (delete unless --dry-run) puts the destructive
//     outcome one forgotten flag away, which is exactly how a bad migration
//     becomes an unrecoverable one.
//   - NO MCP TOOL. Purge is reachable from a shell, not from an LLM caller:
//     internal/mcp/tools.go is deliberately unchanged, so a model cannot reason
//     its way into erasing the episodic tier. If that ever changes, it should
//     be a deliberate decision with its own confirmation design, not a
//     side effect of adding a tool alongside the others.
//
// The tenant is never a flag: the server pins it from the bearer token, so a
// token purges only within its own binding. The token must carry the
// memory-admin role (mint one with `engram token create --roles memory-admin`).
//
// Purge does NOT touch the graph tier — graph rows accumulate source ids from
// many events, so removing one event's contribution is a recompute. Rebuild it
// afterwards with `go run ./cmd/engram-graph-rebuild -tenant <id> -confirm`.

func runPurge(ctx context.Context, args []string, env Env, out io.Writer) error {
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var eventIDs eventIDFlag
	fs.Var(&eventIDs, "event-id", "event id to purge (repeatable; at least one required)")
	confirm := fs.Bool("confirm", false, "actually purge; without it the run is a dry run that mutates nothing")
	addr := fs.String("addr", "", "engramd address")
	token := fs.String("token", "", "bearer token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(eventIDs.ids) == 0 {
		return errors.New("purge: at least one --event-id is required")
	}
	client, err := dialClient(env, *addr, *token)
	if err != nil {
		return err
	}
	defer client.Close()
	res, err := client.MemoryPurge(ctx, eventIDs.ids, !*confirm)
	if err != nil {
		return err
	}
	// Past tense on a real purge, conditional on a dry run — the operator must
	// never have to check which mode produced the numbers in front of them.
	verb, tombstoned := "purged", "soft-deleted"
	if res.DryRun {
		verb, tombstoned = "would purge", "would soft-delete"
	}
	fmt.Fprintf(out, "%s %d event id(s):\n", verb, len(eventIDs.ids))
	fmt.Fprintf(out, "  episodic  %d (hard delete; includes replayed duplicates sharing an event id)\n", res.Episodic)
	fmt.Fprintf(out, "  ledger    %d (hard delete; a re-ingest of these ids will re-extract)\n", res.Ledger)
	fmt.Fprintf(out, "  semantic  %d (%s: expired_at stamped; gone from search, still visible to `engram audit`)\n", res.Semantic, tombstoned)
	if res.DryRun {
		fmt.Fprintln(out, "\ndry run — nothing was changed. Re-run with --confirm to purge.")
		return nil
	}
	fmt.Fprintln(out, "\nthe graph tier was NOT touched; rebuild it with:")
	fmt.Fprintln(out, "  go run ./cmd/engram-graph-rebuild -tenant <id> -confirm")
	// An extraction already in flight when the purge ran will write its ledger
	// row and semantic facts back afterwards (see store.PurgeEvent's limitation
	// note). The operator cannot tell from these counts alone that it happened,
	// so always point at the confirming re-run rather than implying the tiers
	// are now clean.
	fmt.Fprintln(out, "\nif any of these events were ingested moments ago, let extraction settle and")
	fmt.Fprintln(out, "re-run: an in-flight extraction can write rows back after a purge. Purge is")
	fmt.Fprintln(out, "idempotent — repeat until a dry run reports zeros across all three tiers.")
	return nil
}

// eventIDFlag accumulates repeated --event-id values, in the order given.
// Repeating the flag rather than accepting one comma-separated value is
// deliberate: event ids are opaque client-supplied strings that may legally
// contain a comma, and a splitting flag would silently purge two ids the
// operator never named.
type eventIDFlag struct {
	ids []string
}

// String renders the accumulated ids for flag's usage output.
func (f *eventIDFlag) String() string { return strings.Join(f.ids, ",") }

// Set appends one --event-id occurrence. A blank value is refused rather than
// skipped: it almost always means an unset shell variable, and dropping it
// would let the purge report success while missing the row that was meant.
func (f *eventIDFlag) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("--event-id must not be empty")
	}
	f.ids = append(f.ids, v)
	return nil
}
