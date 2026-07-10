# Discovery + Design: Phase 3 - Spill-to-disk overflow at MCP

## Files Found
- `internal/mcp/mcp.go` — protocol server, `Hit`/`Status`/`Backend` types, `Server` (no logger field today).
- `internal/mcp/tools.go` — `callSearch` assembles the `searchResult` via `packSearchResult`; this is where spill hooks in.
- `internal/mcp/budget.go` — `searchResult` envelope (`Hits`, `Omitted`, `OmittedFacets`, `Hint`), `packSearchResult`, `searchByteBudget` (env-var pattern to mirror for the spill-dir env var).
- `internal/mcp/budget_test.go`, `internal/mcp/mcp_test.go` — table-driven + wire-conformance (`refClient`/`fakeBackend`/`fixedHitsBackend`) test patterns to extend.
- `internal/cli/export.go:461` — `writeFileAtomic` (CreateTemp in `dir` + Write + Close + Rename to a caller-supplied final `path`; cleans up the temp file on every error branch). This is the atomic-write precedent to mirror.
- No `internal/mcp/spill.go` yet — new file for this phase.

## Current State
Phase 2 lands `omitted`/`omitted_facets`/`hint` in `searchResult`, computed from the order-preserving split `packed = hits[:N]`, `remainder = hits[N:]`. The union `packed + remainder` is exactly the original `hits` slice `callSearch` already holds — that slice *is* "the full slim result set" Phase 3 needs to spill; no new plumbing is needed to reconstruct it.

There is no logging in `internal/mcp` today. The rest of the codebase (`internal/experience/*`, `internal/authgrpc/interceptor.go`) uses a consistent `*slog.Logger` field defaulting to `slog.Default()` when nil, constructor-injected. `cmd/engram-mcp/main.go` builds `slog.New(slog.NewTextHandler(os.Stderr, nil))` but does not currently pass a logger into `mcp.NewServer`. Wiring a logger through `NewServer` would touch `cmd/engram-mcp/main.go`, which is outside this phase's file scope (`internal/mcp/**`). Using the package-level `slog.Default()`/`slog.Warn(...)` directly avoids that: `slog.Default()`'s initial handler already writes to stderr (same destination `main.go` documents as safe — "stdout is the JSON-RPC channel"), so no stdio corruption risk and no scope creep.

## Gaps
- No spill-dir env var, no atomic spill writer, no `overflow_path` field on `searchResult`.
- `callSearch` doesn't invoke spill; nothing wires `Omitted > 0` to a disk write.
- No test coverage for any of DW-3.1..3.6.

## Code Standards
`docs/code-standards.md` not present in this worktree — followed the codebase's actual conventions instead: doc comments explaining *why* above every exported/significant function (matches `budget.go`'s style exactly), table-driven tests, `DW_N_M`-prefixed test names, dirty tests for every I/O/config boundary, external input (env vars) validated-and-defaulted rather than asserted.

## Test Infrastructure
`internal/mcp` tests run through two seams:
1. Direct unit calls into unexported functions (same package) — used by `budget_test.go` for `packSearchResult`/`topFacets`/`refineHint`.
2. `searchViaWire` in `budget_test.go` — drives `memory_search` through the real JSON-RPC wire via `refClient`/`startServer`, decoding the tool's `content[0].text` JSON. `fixedHitsBackend` (in `budget_test.go`) returns a preset `[]Hit` slice, letting a test control exact hit content/size to force budget overflow deterministically (e.g. via `t.Setenv(searchBudgetBytesEnv, "200")`).

Both seams are directly reusable for Phase 3: unit-test `spillFullResult` directly for the write-path edge cases (permission-denied dir, marshal failure), and use `searchViaWire` + a small env-forced budget for the end-to-end `overflow_path` round-trip.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-3.1 | `omitted>0` → atomic `0600` spill write + absolute `overflow_path`; `omitted==0` → neither | COVERED | `TestDW_3_1_SpillWrittenOnlyWhenOmitted` (subtests: "omitted" and "all fit", each with a per-test `ENGRAM_MCP_SPILL_DIR` so the dir can be globbed for artifacts) |
| DW-3.2 | reading `overflow_path` yields valid JSON unmarshaling to the FULL slim result set | COVERED | `TestDW_3_2_OverflowPathRoundTrips` (wire call with small budget, read the returned path, unmarshal, compare hit IDs to the full input set) |
| DW-3.3 | file mode is exactly `0600` | COVERED | `TestDW_3_3_SpillFileMode0600` (`os.Stat(path).Mode().Perm()`) |
| DW-3.4 | failed spill write degrades gracefully: capped response, no `overflow_path`, warning logged, no panic | COVERED | `TestDW_3_4_UnwritableSpillDirDegradesGracefully` (chmod 0500 dir; swap `slog.Default()` for a buffer-backed logger to assert the warning was logged) |
| DW-3.5 | spill dir `ENGRAM_MCP_SPILL_DIR`-overridable, defaults to OS temp; nonexistent override degrades gracefully | COVERED | `TestDW_3_5_SpillDirOverridable` (subtests: custom dir used; unset falls back to `os.TempDir()`; nonexistent override degrades like DW-3.4) |
| DW-3.6 | write/marshal failure mid-spill leaves NO file renamed into place | COVERED | `TestDW_3_6_MarshalFailureLeavesNoFile` (direct call to `spillFullResult` with an injected `math.NaN()` score forcing `json.Marshal` to fail before any filesystem call; glob the spill dir for zero artifacts) |

**All items COVERED:** YES

## Design Decisions

**Where spill hooks in:** `callSearch`, right after `packSearchResult`. If `result.Omitted > 0`, call `spillFullResult(hits)` (the original, unsliced `hits` slice — already the exact union of packed+remainder per Phase 2's documented invariant, no reconstruction needed). On success, set `result.OverflowPath`; on failure, `slog.Warn(...)` and leave `OverflowPath` unset. This keeps the search call's success path unconditional on spill succeeding (DW-3.4's "must not fail the search").

**Spilled JSON shape:** reuse the existing `searchResult` type (`{Hits: hits}`, everything else zero-value). `omitempty` on `Omitted`/`OmittedFacets`/`Hint`/`OverflowPath` means the spilled file serializes to exactly `{"hits": [...]}"`, no extra envelope noise, and the same type used for wire responses can decode it back — no duplicate schema to maintain.

**Marshal-before-any-I/O ordering:** `spillFullResult` marshals the full content to a `[]byte` *before* calling `os.CreateTemp`. A marshal failure (the only realistic one: a non-finite `float64` `Score`, which `encoding/json` rejects) then never touches the filesystem at all — trivially satisfying DW-3.6's "no file renamed into place," and giving a clean, deterministic way to test it (inject `math.NaN()`) without needing a disk-full simulator.

**Atomic write, mirroring `writeFileAtomic`:** `os.CreateTemp(dir, "engram-mcp-search-*.json.tmp")` → `Write` → `Chmod(0o600)` (explicit, so the guarantee doesn't ride on the process umask even though `CreateTemp`'s own default is already `0600`) → `Close` → `Rename` to `strings.TrimSuffix(tmpName, ".tmp")`. Every error branch removes the temp file before returning, matching `writeFileAtomic`'s cleanup-on-every-branch discipline exactly. Because the final name is derived from the temp name (which `os.CreateTemp` already made unique), no caller-supplied "destination path" is needed the way `writeFileAtomic` takes one for note files — spill files are anonymous scratch files, so deriving the final name is the right adaptation of the precedent, not a deviation from it.

**Absolute path:** `spillDir()` resolves `ENGRAM_MCP_SPILL_DIR` (or `os.TempDir()`) through `filepath.Abs` before use, so both the temp and final names `os.CreateTemp` returns are absolute even when an operator sets a relative override — satisfying "the returned path must be absolute" without a separate resolution step after the fact (which would reopen a race between rename and abs-resolution).

**Logging:** package-level `slog.Warn(...)` (i.e. `slog.Default()`), not a new `Server.logger` field — see Gaps above for why threading a logger through `NewServer` would cross out of this phase's `internal/mcp/**` file scope into `cmd/engram-mcp/main.go`.

**Deliberately not tested (documented, not a gap):** a real "disk full mid-write" `Write()` failure has no deterministic, portable OS-level trigger available in a unit test; the DW item and test plan both accept "write/marshal failure" as satisfying evidence, and the marshal-failure test exercises the identical cleanup code path (`Close`+`Remove` on error, no `Rename` attempted) that a `Write` failure would take — the design does not special-case which step failed.

## Prerequisites
- [x] Phase 2 committed (`packSearchResult`, `searchResult`, order-preserving remainder) — verified in `internal/mcp/budget.go`.
- [x] No missing dependencies; standard library only (`os`, `path/filepath`, `strings`, `log/slog`, `encoding/json`).
- [x] Atomic-write precedent (`internal/cli/export.go:461`) read and confirmed as the pattern to mirror.

## Recommendation
BUILD. The plan fits reality cleanly — Phase 2 already exposes exactly the data Phase 3 needs (the full `hits` slice in `callSearch`, and the order-preserving remainder invariant), and the atomic-write precedent transfers directly. No UPDATE_PLAN needed.
