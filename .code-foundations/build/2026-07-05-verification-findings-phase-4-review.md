# Review: Phase 4 — CLI UX Verification Advisory

## Executed Results (Step 0)

- **Test suite**: `make test` → 6 CLI tests + 48 ingest tests + 55 experience tests = **109 passed**
- **Build**: `go build ./...` → **Success**
- **Typecheck**: `go vet ./...` → **No errors**
- **Lint**: `make lint` (revive) → **Exit code 0 (PASS)**

All suites executed cleanly. No compilation, type, or lint errors introduced.

## Requirement Fulfillment

### DW-4.1
**PREMISE:** ingest warns (advisory, non-fatal, stderr) on a directive-looking line that fails to parse; stays SILENT on well-formed directives AND on plain prose with no directive.

**EVIDENCE:**
- File: `internal/cli/advisory.go:33-51` (directiveAdvisory function)
- File: `internal/cli/cli.go:232-234` (runIngest calls advisory to errW)
- File: `internal/cli/cli_test.go:60-113` (TestRunIngest_AdvisoryOnMalformedDirective, TestRunIngest_SilentOnPlainProse, TestRunIngest_SilentOnWellFormedDirective)

**TRACE:**
1. **Malformed directive triggers advisory:**
   - Input: `"fact: alice prefers dark-mode"` (space-delimited, not pipe-delimited)
   - Execution: Line 41 of advisory.go calls `ingest.IsFactDirectiveLine(trimmed)` → returns true (matches "fact:" prefix)
   - Then calls `!ingest.ParseFactDirectiveLine(trimmed)` → returns true (space-delimited fails pipe parsing)
   - Appends to `bad` slice at line 42
   - Returns advisory string at line 50
   - cli.go line 233: Writes to `errW` (stderr)
   - **Exit code**: 0 (line 68, ingest succeeds)
   - **Stdout**: Contains "ingested" (line 244, client call succeeds)

2. **Plain prose stays silent:**
   - Input: `"Alice mentioned she really prefers dark mode these days."`
   - Execution: Line 36 loops through lines, line 40-45 checks both IsFactDirectiveLine and IsExperienceDirective
   - Neither condition matches (no "fact:" or "experience:" prefix)
   - Loop continues to next (empty) line, then exits
   - Line 47: Returns empty string "" (no bad lines)
   - cli.go line 232: `if msg := directiveAdvisory(*text); msg != "" ` evaluates false
   - No stderr output
   - **Exit code**: 0

3. **Well-formed directive stays silent:**
   - Input: `"fact: alice | prefers | dark-mode\nretract: alice | light-mode\nexperience: fix bug | patch | success | phi=0.7"`
   - Execution: First line, line 41 `IsFactDirectiveLine` true, `ParseFactDirectiveLine(trimmed)` returns true (3 pipe-delimited parts)
   - Not appended to `bad`
   - Second line, `IsFactDirectiveLine` true, `ParseFactDirectiveLine(trimmed)` returns true (2 pipe-delimited parts)
   - Third line, `IsExperienceDirective` true, `ParseExperienceLine(trimmed)` returns true (task + skill both non-empty)
   - Line 47: Returns empty string
   - cli.go line 232: Condition false, no stderr
   - **Exit code**: 0

**VERDICT: PASS**

Test evidence: `TestRunIngest_AdvisoryOnMalformedDirective` (line 64), `TestRunIngest_SilentOnPlainProse` (line 83), `TestRunIngest_SilentOnWellFormedDirective` (line 99). All three passed in test run.

---

### DW-4.2
**PREMISE:** CLI usage/help documents the pipe-delimited grammar AND accurate `--event-id` semantics (test scans help output).

**EVIDENCE:**
- File: `internal/cli/cli.go:71-101` (usage constant)
- File: `internal/cli/cli.go:215-217` (--event-id flag help text)
- File: `internal/cli/cli_test.go:136-173` (TestUsageDocumentsDirectiveGrammar, TestIngestHelpDocumentsEventIDSemantics)

**TRACE:**

1. **Grammar documented:**
   - Usage lines 91-93 show all three directive syntaxes:
     - `fact: subject | predicate | object [@ RFC3339 valid_at]`
     - `retract: subject | predicate [@ RFC3339 valid_at]`
     - `experience: task | distilled skill | success|failure | phi=0..1 | evidence=e1;e2 | signals=s1;s2 | context=...`
   - Test line 143-149 scans help output for each grammar fragment, all present.

2. **--event-id semantics accurate:**
   - Usage lines 97-99 state: "derived semantic facts are deduped by content; the raw episodic log entry for --text, however, is appended on every ingest call — replaying the same --event-id does not deduplicate the raw log."
   - Flag help lines 216-217 repeat: "derived semantic facts are deduped by content, not by this id — the raw episodic log entry is appended on every call regardless"
   - Test line 165-166 verifies help does NOT claim "idempotency event id"
   - Test line 168-169 verifies help contains "deduped by content"
   - Test line 171-172 verifies help contains "appended on every ingest call"

**VERDICT: PASS**

Test evidence: `TestUsageDocumentsDirectiveGrammar` (line 136) and `TestIngestHelpDocumentsEventIDSemantics` (line 158). Both passed.

---

### DW-4.3
**PREMISE:** dirty test — a space-delimited (malformed) `fact:` line triggers the advisory naming the correct grammar; prose-only triggers nothing.

**EVIDENCE:**
- File: `internal/cli/cli_test.go:64-78` (TestRunIngest_AdvisoryOnMalformedDirective)
- File: `internal/cli/cli_test.go:83-94` (TestRunIngest_SilentOnPlainProse)

**TRACE:**

1. **Space-delimited fact line triggers advisory with grammar:**
   - Test line 65: Input `"fact: alice prefers dark-mode"` (malformed: space-separated, not pipe-separated)
   - Assertion line 69-70: stderr contains "warning"
   - Assertion line 72-73: stderr contains `"fact: subject | predicate | object"`
   - Assertion line 75-76: stdout contains "ingested" (proof advisory doesn't block)
   - Assertion line 66-67: exit code 0

2. **Prose-only triggers nothing:**
   - Test line 84: Input `"Alice mentioned she really prefers dark mode these days."`
   - Assertion line 88-89: stderr is completely empty (`errW != ""` fails)
   - Assertion line 91-92: stdout contains "ingested" (ingest proceeds)
   - Assertion line 85-86: exit code 0

**VERDICT: PASS**

Executed test results confirm both cases: TestRunIngest_AdvisoryOnMalformedDirective PASS, TestRunIngest_SilentOnPlainProse PASS.

---

### DW-4.4
**PREMISE:** `make test`/`make lint` green; no change to successful-ingest behavior.

**EVIDENCE:**
- Step 0 execution above: All tests passed, all linting passed
- File: `internal/cli/cli_test.go:75-76, 91-92, 110-111` (all verify "ingested" in stdout on successful advisory cases)
- File: `internal/cli/cli.go:240-244` (IngestScoped call and output happen regardless of advisory)

**TRACE:**
- `make test`: 109 tests across 3 packages, 0 failures
- `make lint`: Exit code 0, no violations
- Successful ingest: runIngest (line 212-246) calls directiveAdvisory (line 232), writes to errW if non-empty (line 233), but immediately continues to dialClient, IngestScoped, and success output (lines 235-244). Advisory is non-blocking and purely additive.

**VERDICT: PASS**

All test suites execute cleanly. No new test failures. Advisory does not interfere with successful ingest (all 4 CLI tests that ingest data report exit code 0 and "ingested" in output).

---

## Test-DW Coverage

| DW Item | Test Name | Status |
|---------|-----------|--------|
| DW-4.1 (malformed triggers advisory) | TestRunIngest_AdvisoryOnMalformedDirective | ✓ Executed, PASS |
| DW-4.1 (prose stays silent) | TestRunIngest_SilentOnPlainProse | ✓ Executed, PASS |
| DW-4.1 (well-formed stays silent) | TestRunIngest_SilentOnWellFormedDirective | ✓ Executed, PASS |
| DW-4.1 (mixed content names each bad line) | TestRunIngest_AdvisoryNamesEachMalformedLine | ✓ Executed, PASS |
| DW-4.2 (grammar documented) | TestUsageDocumentsDirectiveGrammar | ✓ Executed, PASS |
| DW-4.2 (event-id semantics accurate) | TestIngestHelpDocumentsEventIDSemantics | ✓ Executed, PASS |
| DW-4.3 (space-delimited malformed) | TestRunIngest_AdvisoryOnMalformedDirective | ✓ (sub-case of DW-4.1) |
| DW-4.3 (prose-only triggers nothing) | TestRunIngest_SilentOnPlainProse | ✓ (sub-case of DW-4.1) |
| DW-4.4 (test/lint green) | All test suites | ✓ 109 PASS, 0 FAIL |

**Coverage**: 100% — all DW items have automated tests with passing execution evidence.

---

## Dead Code

Scanned for:
- Unreachable code after early returns: **None found**
- Commented-out code blocks: **None found**
- Debug statements (TEMP:, println, log, etc): **None found**
- Unused imports: **None found** (ingest, experience, fmt, strings all used)
- Empty catch blocks: **N/A** (Go uses error returns, not exceptions)
- Unused helper functions: **None** (directiveAdvisory, factPrefix, malformedLine all called)

**Verdict: No dead code issues.**

---

## Correctness Dimensions

| Dimension | Status | Evidence |
|-----------|--------|----------|
| **Concurrency** | PASS | Single-threaded ingest flow. No shared mutable state in advisory code. Each ingest call gets its own directiveAdvisory invocation with local `bad` slice. |
| **Error Handling** | PASS | Advisory is purely informational (never blocks or changes outcome). External input (--text) is scanned but never parsed in-process; real parsing delegated to gRPC server via ingest.ParseFactDirectiveLine/experience.ParseExperienceLine. No I/O or external calls in advisory path. |
| **Resources** | PASS | Advisory allocates a single `bad` slice, grows by append (O(n) text lines). No file handles, connections, locks, or caches. Memory freed on return. |
| **Boundaries** | PASS | Input `text` is a string from command-line flag. Split into lines, each trimmed and checked for directive prefix (string operations only). No integer overflow, null pointers, or out-of-bounds access. Slice bounds checked (`for i, line := range strings.Split`). |
| **Security** | PASS | Advisory never executes untrusted input. --text is scanned for patterns only, never parsed or evaluated. Grammar detection is pattern-matching on a known set of prefixes ("fact:", "retract:", "experience:"). Real parsing delegated to known, tested functions. No injection risk. |

---

## Loaded-Skill Criteria

### Skill: code-foundations:cc-defensive-programming

| Criterion | Status | Evidence |
|-----------|--------|----------|
| **No executable code in assertions** | PASS | No assertions in new code (advisory.go, changes to cli.go). |
| **No empty catch blocks** | PASS | No exception handling in advisory code (Go uses error returns). |
| **External input validated at entry** | PASS | Advisory processes --text (external, from command-line flag) but never executes it. Parsing delegated to tested functions (ingest.ParseFactDirectiveLine, experience.ParseExperienceLine). Advisory only flags patterns for user information; no decision made based on untrusted parsing. |
| **Assertions for bugs only, not anticipated errors** | PASS | No assertions. Advisory code has no anticipated errors (it processes known-good string splits and prefix checks). |

**Verdict: PASS** — Advisory follows defensive patterns: it validates by delegation (calls real parsers), never makes decisions based on unvalidated input, and safely processes strings without assertions or exceptions.

---

### Skill: code-foundations:code-clarity-and-docs

| Criterion | Status | Evidence |
|-----------|--------|----------|
| **All public entities documented** | PASS | Functions in advisory.go and new exported functions in rule.go/distill.go are all commented: `directiveAdvisory` (lines 21-32), `factPrefix` (lines 53-55), `malformedLine` (lines 63-64), `IsFactDirectiveLine` (lines 92-96), `ParseFactDirectiveLine` (lines 107-112), `IsExperienceDirective` (lines 42-46), `ParseExperienceLine` (lines 51-57). |
| **Comments use different words than code** | PASS | Comments explain *why* (e.g., "Plain prose is the correct, expected input for the production LLM extractor" lines 28-30) not just *what*. They name the delegation pattern ("delegates to the same parseDirective call" line 23) and design rationale ("so the message and the parser can never drift apart" line 14). |
| **Interface vs implementation comments correct** | PASS | directiveAdvisory comment (lines 21-32) is an interface comment: it says what the function does, when it fires/stays silent, and returns. factPrefix and malformedLine are implementation helpers (lines 53-55, 63-64) with brief, focused comments. No implementation leaks into interface docs. |
| **Variable comments complete** | PASS | Local variables `bad` (slice of strings, line 34) and loop vars `i, line` are standard; no special units/ownership/invariants to document. The directiveGrammar map (lines 15-19) has a clear comment explaining its role ("Used only to render the advisory message"). |
| **README/CLAUDE.md/help text accurate** | PASS | Help text (cli.go lines 91-99) updated to document directives and advisory behavior. No stale or contradictory documentation. |

**Verdict: PASS** — Code clarity is high. Comments explain design rationale and delegation, not just restating code. All public APIs documented with interface comments that distinguish from implementation.

---

## Notes (non-blocking)

1. **Grammar display in advisory.go** — The `directiveGrammar` map (lines 15-19) includes detailed syntax (e.g., `phi=0..1`, `evidence=...`) that mirrors internal/experience/distill.go's parseExperienceDirective, not a separate "grammar spec". This is correct for display but maintainers should note that the map is documentation only; the real grammar is defined in the respective parsers.

2. **IsExperienceDirective retrims input** — The function (experience/distill.go:47-49) applies `strings.TrimSpace` even though callers (advisory.go:43) already pass trimmed strings. This is fine (idempotent and defensive) but adds a small redundancy. Not a defect, just a minor inefficiency.

3. **Advisory message format** — The message format ("line X looks like a 'fact:' directive...") is user-friendly and includes the input line for context. No clarity issues.

4. **Test coverage of edge cases** — Tests cover malformed, prose-only, and well-formed cases. Additional edge cases (e.g., directive prefix in the middle of prose, like "the fact: is that...") are correctly NOT flagged because IsFactDirectiveLine requires the prefix at the start after trimming. This is correct behavior.

---

## Issues

**None. All requirements met with execution evidence.**

---

## Summary

- **All DW items PASS** with automated test evidence
- **Test coverage 100%** — every requirement has one or more passing test
- **Build, typecheck, lint all green** — no regressions
- **Defensive programming**: Advisory never executes untrusted input; delegates parsing to tested functions
- **Code clarity**: Interface comments explain rationale, not just restate code
- **Correctness**: Verified concurrency (none), error handling (non-blocking), resources (none), boundaries (safe), security (pattern-match only, no execution)

**Verdict: POST-GATE PASS**

