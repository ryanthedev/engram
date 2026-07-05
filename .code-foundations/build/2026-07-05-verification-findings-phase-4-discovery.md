# Discovery + Design: Phase 4 - CLI ingest UX

## Files Found
- `internal/cli/cli.go` — the entire CLI package (single file, no existing tests). `Run` dispatches
  `runIngest(ctx, rest, env, out)` — note it is NOT currently passed `errW`, unlike `runToken`.
  `usage` const documents `engram ingest ... --event-id ID --text TEXT ...` with no grammar hint.
  `--event-id` flag help says `"idempotency event id (required)"`.
- `internal/ingest/rule.go` — `RuleExtractor` / `synthesizeWire` / `parseDirective(body string, n int) (wireFact, bool)`
  (unexported). Recognizes `fact:` (n=3) and `retract:` (n=2) line prefixes, pipe-delimited body,
  optional `@ RFC3339` suffix. `parseDirective` already discriminates "too few pipe parts" (ok=false)
  from a directive that parses.
- `internal/ingest/extraction.go` — `ParseExtraction`, `ErrMalformed`, `ErrNoFacts`, `wireFact` (the
  barricade both `RuleExtractor` and `HTTPExtractor` funnel through).
- `internal/experience/distill.go` — `RuleDistiller.Distill(text)` finds the first `experience:` line
  and hands its body to `parseExperienceDirective(body) Experience` (unexported, no failure mode today
  — any body, even zero pipes, produces *some* `Experience`; well-formedness per the documented grammar
  `<task> | <distilled skill> | ...` requires at least a non-empty task AND distilled-skill field).
- No `internal/cli/*_test.go`, `internal/ingest/rule_test.go` exist yet — this phase adds the first CLI
  tests and the first rule.go-specific test file.

## Current State
- `engram ingest --event-id ID --text TEXT` sends `TEXT` verbatim over gRPC; the async extraction
  pipeline (RuleExtractor in fixtures / HTTPExtractor in production) later turns directive lines into
  facts. A malformed directive line (e.g. `fact: alice loves cats` — no pipes) is silently dropped by
  `parseDirective` (ok=false → the line contributes nothing to `wire`), and if it's the only line, the
  extraction rejects with `ErrNoFacts` deep in the async worker — the CLI operator never sees why.
- `--event-id` help text overstates dedup: only derived semantic facts are content-key deduped; the
  raw episodic log itself is appended per call, not deduplicated by event-id.
- The CLI has zero test coverage today (no `internal/cli/*_test.go`).

## Gaps
- `runIngest` doesn't receive `errW`, so there is currently no path to print a non-fatal stderr
  advisory from inside it. `Run`'s dispatch line must be updated to pass `errW` through (mirrors the
  existing `runToken(ctx, rest, env, out, errW)` pattern already in the file — so this is following an
  established convention, not inventing one).
- No exported predicate exists in `internal/ingest` or `internal/experience` for "is this a directive
  line" / "does it parse" — the plan explicitly requires reusing these authoritative parsers rather
  than duplicating grammar in the CLI.
- `experience:` grammar has no explicit "well-formed" concept today (parseExperienceDirective never
  fails) — Phase 4 needs to define malformed as "task or distilled-skill field empty after parsing",
  matching the documented `<task> | <distilled skill> | ...` grammar in distill.go's doc comment.

## Code Standards
- Go 1.23+, `internal/` packages, one bounded concern per package (from `docs/code-standards.md`).
- Errors returned, not panicked; sentinel errors + `errors.Is`/`errors.As`.
- Table-driven tests; every code-touching phase ships at least one error-path ("dirty") test.
- Structured logs via `log/slog` — not applicable to this CLI-only phase (no logger threaded into
  `cli.Run`; the advisory is a plain stderr print, consistent with how `runIngest`/`runToken` already
  report errors via `fmt.Fprintln(errW, ...)` in `Run`).

## Test Infrastructure
- `go test ./...` (`make test`), `go vet` + `revive` (`make lint`).
- CLI has no test harness yet; `cli.Run(ctx, args, env, out, errW)` is already designed to be driven
  headlessly (explicit streams, `Env` func for env vars) — tests will call `cli.Run` directly with a
  `bytes.Buffer` for `out`/`errW`, no real gRPC dial needed for the flag-parsing/advisory paths (the
  advisory must fire before/independent of the network dial, since a malformed directive doesn't
  require a live server to detect).
- `internal/ingest` and `internal/experience` already have `_test` external test packages
  (`package ingest_test`, presumably `package experience` internal for distill_test.go — confirmed:
  `distill_test.go` uses `package experience`, same-package white-box tests).

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-4.1 | ingest command warns (advisory, non-fatal, stderr) on a directive-looking line that fails to parse; silent on well-formed directives AND plain prose | COVERED | `TestRunIngest_AdvisoryOnMalformedDirective`, `TestRunIngest_SilentOnWellFormedDirective`, `TestRunIngest_SilentOnPlainProse` in `internal/cli/cli_test.go` |
| DW-4.2 | CLI usage/help text documents pipe-delimited grammar AND accurate `--event-id` semantics | COVERED | `TestUsageDocumentsDirectiveGrammar`, `TestIngestHelpDocumentsEventIDSemantics` in `internal/cli/cli_test.go` |
| DW-4.3 | dirty test: space-delimited malformed `fact:` line → advisory naming correct grammar; prose-only → nothing | COVERED | `TestRunIngest_AdvisoryOnMalformedDirective` (asserts message names `fact: subject \| predicate \| object` grammar), `TestRunIngest_SilentOnPlainProse` |
| DW-4.4 | `make test`/`make lint` green; no change to successful-ingest behavior | COVERED | full suite run (`go test ./...`, `go vet`, `revive`) + `TestRunIngest_AdvisoryDoesNotBlockIngest` (advisory prints but ingest still proceeds/succeeds) |

**All items COVERED:** YES

## Design Decisions

**Reuse surface (no grammar duplication):**
- `internal/ingest/rule.go` gets two new exported functions built directly on the existing unexported
  `parseDirective`:
  - `IsFactDirectiveLine(line string) bool` — trims and checks the `fact:`/`retract:` prefixes (the
    same two prefixes `synthesizeWire` already switches on).
  - `ParseFactDirectiveLine(line string) bool` — dispatches to `parseDirective` with the same `n` per
    prefix (3 for fact, 2 for retract) that `synthesizeWire` already uses, returning ok/not-ok. Zero
    grammar duplicated; it is a thin exported wrapper around what already exists.
- `internal/experience/distill.go` gets:
  - `IsExperienceDirective(line string) bool` — trims and checks the `experience:` prefix (mirrors
    `Distill`'s own check).
  - `ParseExperienceLine(line string) bool` — calls the existing unexported `parseExperienceDirective`
    and reports well-formed = both `Task` and `DistilledSkill` non-empty after trim (the two positional
    fields the doc comment's grammar requires before the optional `key=value` fields).
- Both packages already export `Extractor`/`Distiller`/parse-adjacent surface — these two additions
  are the same shape (small bool-returning predicates), not a new architectural layer.

**CLI advisory (`internal/cli/advisory.go`, new file):**
- `directiveAdvisory(text string) string` — splits `text` on `\n`, trims each line, skips blanks. For
  each non-blank line: if `ingest.IsFactDirectiveLine(line)` and `!ingest.ParseFactDirectiveLine(line)`,
  or `experience.IsExperienceDirective(line)` and `!experience.ParseExperienceLine(line)`, collect it as
  malformed. Plain prose (no directive prefix matched) is never collected — silence is the explicit
  contract (production `HTTPExtractor` input is prose; warning here would cry wolf on every real
  ingest). Returns `""` when nothing is malformed (so callers can `if msg := directiveAdvisory(text); msg != "" { ... }`).
- Message format names the exact pipe grammar per directive kind so the DW-4.3 "advisory naming the
  correct grammar" requirement is testable by substring match, e.g.:
  `engram: warning: line 2 looks like a "fact:" directive but does not parse — expected "fact: subject | predicate | object [@ RFC3339]"`.
- `runIngest` signature gains `errW io.Writer` (mirrors `runToken`'s existing param order); `Run`'s
  dispatch line changes from `runIngest(ctx, rest, env, out)` to `runIngest(ctx, rest, env, out, errW)`.
  The advisory check runs after flag validation but BEFORE the network dial (`dialClient`) — it needs
  no live server, and firing first means a malformed directive is flagged even if the dial later fails
  for unrelated reasons. It never blocks or changes the ingest outcome (constraint: "no false
  synchronous guarantee" — it's a client-side lint, not a gate).

**Usage/help text changes:**
- `usage` const: add a short grammar block near the `engram ingest` line documenting
  `fact: subject | predicate | object [@ RFC3339]`, `retract: subject | predicate [@ RFC3339]`,
  `experience: task | distilled skill | success|failure | ...` — and a one-line note that these are
  optional; plain prose is also valid and is what the production extractor expects.
- `--event-id` flag help changes from `"idempotency event id (required)"` to language that
  distinguishes: derived semantic facts are content-key deduped, but the raw episodic log entry is
  appended every call (not deduped by event-id). Kept short since `flag` help strings render inline,
  but precise — no "idempotency" word implying full dedup.

**Why not validate synchronously against the server:** Out of scope per the plan (no wire-protocol
change, no false synchronous guarantee) — the advisory is purely a client-side static check against
the same grammar the async extractor uses, so CLI and server agree on "well-formed" without adding a
round trip.

## Prerequisites
- [x] Required files exist: `internal/cli/cli.go`, `internal/ingest/rule.go`, `internal/experience/distill.go`.
- [x] Dependencies available: no new imports beyond `internal/ingest` and `internal/experience` in the
  CLI package (already imports `internal/experience` for the quarantine store) — `internal/ingest` is a
  new import for `internal/cli`.
- [x] No missing prerequisites; Phase 4 depends on none of the other phases (plan states `Depends on: none`).

## Recommendation
BUILD.

The plan fits reality closely: the two authoritative parsers named in the plan
(`internal/ingest/rule.go`, `internal/experience/distill.go`) are exactly where the grammar lives, and
`parseDirective`/`parseExperienceDirective` are trivially wrappable into exported predicates without
duplicating logic. The only design decision the plan left implicit — what "well-formed" means for an
`experience:` line, since `parseExperienceDirective` has no native failure mode — is resolved above
(non-empty task + distilled-skill). One structural fix needed beyond what the plan states outright:
`runIngest` must gain an `errW` parameter (currently only `out`) to have anywhere to print the stderr
advisory; this is a mechanical, in-scope change to `internal/cli/cli.go` already covered by the phase's
file scope.
