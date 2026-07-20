// seedknowledge.go implements the `engram-seed-knowledge` core routine (Phase
// 3 of the memory-knowledge mapping prototype): rtd has zero knowledge
// collections, so the knowledge->vault export renderer (vaultknowledge.go)
// has nothing to render without a seeded collection. This routine creates a
// public `curated_notes` collection with a `memory_ref`/`memory_ref_name`
// mapping and ingests a fixed demo-doc set whose memory_refs point at real
// rtd memory entity ids, so the next `engram export` wikilinks them straight
// into the memory vault.
//
// Both operations require the admin/harvester role (internal/knowledgeauth) —
// mint a role-bearing token first via `engram token create --roles admin`
// (Phase 1). A role-less or missing token surfaces the server's
// PermissionDenied wrapped with that remedy, so the failure is
// self-explanatory instead of a bare gRPC status.
//
// Idempotency: CreateCollection is tolerated when the collection already
// exists (codes.AlreadyExists) so re-running the tool never fails on create;
// KnowledgeIngest always re-runs and upserts every demo doc by id, so a
// re-run converges rather than duplicating.

package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/ryanthedev/engram/internal/engramclient"
	"github.com/ryanthedev/engram/internal/mcp"
)

// seedCollectionName, seedSource, and seedHarvestID are stable across runs:
// re-running the tool always targets the same collection and the same
// (source, harvest_id) pair, which is what makes KnowledgeIngest's per-id
// upsert converge instead of accumulating duplicate batches.
const (
	seedCollectionName = "curated_notes"
	seedSource         = "curated-demo"
	seedHarvestID      = "curated-demo-seed"
)

// seedDemoDocs is the single source of the curated demo-doc set (DW-3.2):
// every doc's Fields carries a non-empty memory_ref (a real rtd memory
// entity id) and memory_ref_name, so each renders as a resolving [[wikilink]]
// on export -- except curated-unresolved-demo, whose memory_ref is a
// deliberately non-existent id, demonstrating Phase 2's inert
// unresolved-marker path live. Both memory_ref and title/text are
// UNTRUSTED-shaped values as far as vaultknowledge.go is concerned once they
// round-trip through the server, but the literal prose here is authored, not
// harvested -- ordinary curated content, not adversarial input.
var seedDemoDocs = []mcp.KnowledgeDoc{
	{
		ID:    "curated-upublish",
		Title: "upublish",
		Text: "upublish is the one-command static-site publisher this workspace uses to ship preview and " +
			"production builds without hand-rolling deploy scripts. It wraps namespace, versioning, and " +
			"passcode-gated preview links behind a small CLI surface.",
		Fields: map[string]any{
			"memory_ref":      "1806bcd621ed282cf62f136f986a700db229c8dc85fc779aae05444bbd7f7688",
			"memory_ref_name": "upublish",
		},
	},
	{
		ID:    "curated-mcp-browser",
		Title: "mcp-browser",
		Text: "mcp-browser gives an agent persistent, scriptable control of a real Chrome instance over CDP -- " +
			"navigation, DOM/accessibility reads, form filling, network interception, and Lighthouse audits -- " +
			"so browser automation survives across tool calls instead of restarting each time.",
		Fields: map[string]any{
			"memory_ref":      "291361ccd4000a3079d72ed8ee2484986dede0b865630db9dd409b4ee36243cf",
			"memory_ref_name": "mcp-browser",
		},
	},
	{
		ID:    "curated-design-foundations",
		Title: "design-foundations",
		Text: "design-foundations is the pixels-first design workflow: a micro-brief narrows scope, dealt " +
			"reference-collision directions get rendered as specimen tiles, and the user converges on one " +
			"direction before it's locked into a DESIGN.md and handed to build.",
		Fields: map[string]any{
			"memory_ref":      "0fe4ede814c24b67b728094d5fd020e31ddc8916ccd70b9be92918579deeafbd",
			"memory_ref_name": "design-foundations",
		},
	},
	{
		ID:    "curated-claude-code",
		Title: "Claude Code",
		Text: "Claude Code is the agentic CLI this whole build runs inside: it drives file edits, shell " +
			"commands, and subagent dispatch directly from the terminal, with skills and MCP servers extending " +
			"what a single turn can do.",
		Fields: map[string]any{
			"memory_ref":      "0f3913311714b9209959622b4a83e3a29a08948e3a3bcca06f2b6afd4c19e163",
			"memory_ref_name": "Claude Code",
		},
	},
	{
		ID:    "curated-shared-design-system",
		Title: "SHARED DESIGN SYSTEM",
		Text: "The SHARED DESIGN SYSTEM is the cross-project token and component baseline other surfaces are " +
			"built against, so a new page starts from an already-consistent visual language instead of " +
			"reinventing spacing, color, and type scales each time.",
		Fields: map[string]any{
			"memory_ref":      "7e679dac89bfcb3d68e5d4bda5f90e321238669bf7f155e40693ac18ba03fefc",
			"memory_ref_name": "SHARED DESIGN SYSTEM",
		},
	},
	{
		ID:    "curated-unresolved-demo",
		Title: "a deliberately unmapped topic",
		Text: "This doc's memory_ref intentionally points at an entity id that does not exist in the exported " +
			"graph, so the knowledge renderer's unresolved-marker path (an inert line, no dangling wikilink, no " +
			"backlink) is exercised live rather than only in unit tests.",
		Fields: map[string]any{
			"memory_ref":      "entity-does-not-exist-000",
			"memory_ref_name": "a deliberately unmapped topic",
		},
	},
}

// seedCollectionSpec is the curated_notes collection spec: public (so the
// ordinary role-less export identity can read it -- only seeding needs the
// admin token) with memory_ref declared filterable/sortable (future
// structured lookups) and memory_ref_name a plain keyword.
func seedCollectionSpec() mcp.CollectionSpec {
	return mcp.CollectionSpec{
		Name:      seedCollectionName,
		TextField: "text",
		Public:    true,
		Mappings: map[string]mcp.FieldSpec{
			"memory_ref":      {Type: "keyword", Filterable: true, Sortable: true},
			"memory_ref_name": {Type: "keyword"},
		},
	}
}

// RunSeedKnowledge parses `-addr`/`-token` (mirroring runExport's flag
// convention, with the usual ENGRAM_ADDR/ENGRAM_TOKEN env fallback via
// dialClient), dials engramd, and runs the seed routine.
func RunSeedKnowledge(ctx context.Context, args []string, env Env, out io.Writer) error {
	flags := flag.NewFlagSet("engram-seed-knowledge", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	addr := flags.String("addr", "", "engramd address")
	token := flags.String("token", "", "bearer token (admin/harvester role required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("seed-knowledge: unexpected arguments: %v", flags.Args())
	}

	client, err := dialClient(env, *addr, *token)
	if err != nil {
		return err
	}
	defer client.Close()

	return seedKnowledge(ctx, client, out)
}

// seedKnowledge creates (or tolerates the existing) curated_notes collection
// and ingests seedDemoDocs. Split out from RunSeedKnowledge so tests can
// drive it directly against an already-dialed stub client.
func seedKnowledge(ctx context.Context, client *engramclient.Client, out io.Writer) error {
	if err := createSeedCollection(ctx, client); err != nil {
		return err
	}
	indexed, err := client.KnowledgeIngest(ctx, seedCollectionName, seedSource, seedHarvestID, seedDemoDocs)
	if err != nil {
		return wrapPermissionDenied("ingesting demo docs into "+seedCollectionName, err)
	}
	fmt.Fprintf(out, "seeded %s: %d demo docs ingested (source=%s harvest_id=%s)\n",
		seedCollectionName, indexed, seedSource, seedHarvestID)
	return nil
}

// createSeedCollection registers curated_notes, tolerating the
// already-exists conflict a re-run produces (idempotency, DW-3.1) --
// anything else, notably PermissionDenied, is a real failure.
func createSeedCollection(ctx context.Context, client *engramclient.Client) error {
	err := client.CreateCollection(ctx, seedCollectionSpec())
	if err == nil || engramclient.IsAlreadyExists(err) {
		return nil
	}
	return wrapPermissionDenied("creating the "+seedCollectionName+" collection", err)
}

// wrapPermissionDenied names the `--roles admin` remedy on a PermissionDenied
// failure (DW-3.3) so a missing/role-less token produces a self-explanatory
// error instead of a bare gRPC status; every other error (including a nil
// err, which this never receives) passes through untouched, since only the
// permission-denied path has a known, actionable fix.
func wrapPermissionDenied(action string, err error) error {
	if engramclient.IsPermissionDenied(err) {
		return fmt.Errorf("seed-knowledge: %s: permission denied -- mint an admin token first: `engram token create --roles admin` (%w)", action, err)
	}
	return err
}
