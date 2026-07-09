//go:build smoke

package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// claudeMdLeakMarkers are phrases that would only appear in the judge's
// output if it pulled content from the user's injected global
// ~/.claude/CLAUDE.md / RTK.md (per the research's confirmed gotcha) rather
// than reconciling strictly against the given SOURCE EVENT and candidate
// sets. None of these terms appear anywhere in this test's sample source
// event or candidate sets, so their presence in the judge's output is
// unambiguous evidence of a leak. "rtd" itself is deliberately NOT a marker
// here — it is also the legitimate subject of this test's sample event
// (mirroring smoke_test.go's existing convention), so it cannot distinguish
// a leak from a faithful extraction.
var claudeMdLeakMarkers = []string{
	"claude.md",
	"first principles",
	"rust token killer",
	"omniping.dev",
	"orchestration",
	"global instructions",
	"never assume. never guess. never lie",
	"rtk gain",
}

// TestDW_1_5_LiveSmokeJudgeGuardsAgainstCLAUDEMdLeak is the DW-1.5 live
// guard: a real `claude --model sonnet` judge reconciles two known candidate
// sets (one carrying a deliberately unfaithful fact) against a source event
// with known triples, and the result must (a) be faithful to the source
// event and (b) carry NO trace of the judge's injected global CLAUDE.md/
// RTK.md content — proving the strict judgeSystemPrompt holds against the
// real CLI, not just in the hermetic fakes.
//
// Gated behind the `smoke` build tag (this repo's established pattern for
// tests needing a live external resource — see smoke_test.go's DW-1.6 live
// agy test) so it never runs as part of `go test ./...` / `make test`. It
// skips cleanly with a reason when claude isn't on PATH, and ALSO skips
// (rather than silently passing) if the live call itself errors — e.g. an
// unauthenticated CLI — per the plan's explicit instruction never to fake a
// live result. Run it explicitly:
//
//	go test -tags=smoke ./cmd/engram-extract-shim/... -run TestDW_1_5_LiveSmokeJudgeGuardsAgainstCLAUDEMdLeak -v
//
// or via `make smoke-extract-shim-ensemble-judge`.
func TestDW_1_5_LiveSmokeJudgeGuardsAgainstCLAUDEMdLeak(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not found on PATH — DW-1.5 live guard cannot run in this environment; see the plan's note on DW-1.5 for how to run it manually once claude is installed and authenticated")
	}

	const sourceEvent = "[event evt-smoke-judge-1 kind=note at=2026-07-08T12:00:00Z]\nrtd prefers tabs over spaces in Go code. rtd's preferred shell is zsh.\n\n"

	// agy's candidate set deliberately carries one unfaithful fact (not
	// supported by sourceEvent) so this test doubles as a live faithfulness
	// check: the judge should drop it, not just avoid CLAUDE.md leakage.
	agyCandidates := `[{"subject":"rtd","predicate":"prefers","object":"tabs"},{"subject":"rtd","predicate":"lives in","object":"a data center on Mars"}]`
	codexCandidates := `[{"subject":"rtd","predicate":"prefers","object":"tabs"},{"subject":"rtd","predicate":"uses","object":"zsh"}]`

	e := ensembleBackend{
		Agy:   fakeCandidate(agyCandidates, nil),
		Codex: fakeCandidate(codexCandidates, nil),
		Judge: claudeBackend{Model: judgeModel},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// systemPrompt is forwarded only to Agy/Codex (here fake closures that
	// ignore their inputs) — the judge always uses its own internal
	// judgeSystemPrompt, never the caller-supplied one, so its value here is
	// irrelevant to what's under test.
	out, err := e.Run(ctx, "unused-by-fakes", sourceEvent)
	if err != nil {
		t.Skipf("DW-1.5 live judge call failed (claude may not be authenticated) — skipping rather than claiming a result: %v", err)
	}

	canonical, count := parseFacts(out)
	if count == 0 {
		t.Fatalf("DW-1.5 live judge returned zero facts for a source event carrying two clear durable facts; output=%s", out)
	}

	lower := strings.ToLower(string(canonical))
	for _, marker := range claudeMdLeakMarkers {
		if strings.Contains(lower, marker) {
			t.Fatalf("DW-1.5 FAILED: judge output contains CLAUDE.md/global-config-derived marker %q — leak detected: %s", marker, canonical)
		}
	}

	// Faithfulness: the reconciled output must trace back to the source
	// event's real facts (rtd/tabs/spaces/zsh) and must NOT retain the
	// deliberately unfaithful "Mars" candidate fact.
	if strings.Contains(lower, "mars") {
		t.Fatalf("DW-1.5 FAILED: judge retained the deliberately unfaithful candidate fact instead of dropping it: %s", canonical)
	}
	if !strings.Contains(lower, "tab") && !strings.Contains(lower, "zsh") {
		t.Fatalf("DW-1.5 FAILED: judge output does not trace back to the source event's real facts (tabs/zsh): %s", canonical)
	}

	t.Logf("DW-1.5 live smoke: judge reconciled faithfully with no CLAUDE.md leak: %s", canonical)
}
