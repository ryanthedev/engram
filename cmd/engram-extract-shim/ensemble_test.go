package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeCandidate returns a fakeBackend (the existing test-only Strategy
// implementation from server_test.go) that always returns raw on success, or
// errors when err is non-nil.
func fakeCandidate(raw string, err error) Backend {
	return fakeBackend{run: func(context.Context, string, string) (string, error) {
		if err != nil {
			return "", err
		}
		return raw, nil
	}}
}

// capturingBackend records the systemPrompt/userPrompt it was invoked with,
// alongside a fakeBackend's canned response — used where a test needs to
// inspect exactly what the ensemble handed a leg (DW-1.2, DW-1.6).
type capturingBackend struct {
	raw       string
	err       error
	gotSystem *string
	gotUser   *string
}

func (c capturingBackend) Run(_ context.Context, systemPrompt, userPrompt string) (string, error) {
	if c.gotSystem != nil {
		*c.gotSystem = systemPrompt
	}
	if c.gotUser != nil {
		*c.gotUser = userPrompt
	}
	if c.err != nil {
		return "", c.err
	}
	return c.raw, nil
}

func decodeFacts(t *testing.T, raw string) []fact {
	t.Helper()
	var facts []fact
	if err := json.Unmarshal([]byte(raw), &facts); err != nil {
		t.Fatalf("output not a valid fact array: %v (%s)", err, raw)
	}
	return facts
}

// TestDW_1_1_EnsembleHappyPath_TableDriven is the table-driven happy-path
// test DW-1.1 asks for: fake agy + fake codex + fake judge, asserting the
// ensemble's Run returns exactly the judge's reconciled array.
func TestDW_1_1_EnsembleHappyPath_TableDriven(t *testing.T) {
	tests := []struct {
		name      string
		agyOut    string
		codexOut  string
		judgeOut  string
		wantFacts []fact
	}{
		{
			name:      "judge merges and returns one fact",
			agyOut:    `[{"subject":"rtd","predicate":"prefers","object":"tabs"}]`,
			codexOut:  `[{"subject":"rtd","predicate":"prefers","object":"tabs"}]`,
			judgeOut:  `[{"subject":"rtd","predicate":"prefers","object":"tabs"}]`,
			wantFacts: []fact{{Subject: "rtd", Predicate: "prefers", Object: "tabs"}},
		},
		{
			name:      "judge drops an unfaithful candidate fact",
			agyOut:    `[{"subject":"rtd","predicate":"prefers","object":"tabs"},{"subject":"rtd","predicate":"lives in","object":"mars"}]`,
			codexOut:  `[{"subject":"rtd","predicate":"prefers","object":"tabs"}]`,
			judgeOut:  `[{"subject":"rtd","predicate":"prefers","object":"tabs"}]`,
			wantFacts: []fact{{Subject: "rtd", Predicate: "prefers", Object: "tabs"}},
		},
		{
			name:      "judge legitimately returns empty",
			agyOut:    `[{"subject":"rtd","predicate":"maybe","object":"unclear"}]`,
			codexOut:  `[]`,
			judgeOut:  `[]`,
			wantFacts: []fact{},
		},
		{
			name:      "judge output fenced — barricade unwraps it",
			agyOut:    `[{"subject":"rtd","predicate":"uses","object":"zsh"}]`,
			codexOut:  `[{"subject":"rtd","predicate":"uses","object":"zsh"}]`,
			judgeOut:  "```json\n[{\"subject\":\"rtd\",\"predicate\":\"uses\",\"object\":\"zsh\"}]\n```",
			wantFacts: []fact{{Subject: "rtd", Predicate: "uses", Object: "zsh"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := ensembleBackend{
				Agy:   fakeCandidate(tc.agyOut, nil),
				Codex: fakeCandidate(tc.codexOut, nil),
				Judge: fakeCandidate(tc.judgeOut, nil),
			}
			out, err := e.Run(context.Background(), "sys", "rtd prefers tabs")
			if err != nil {
				t.Fatalf("Run() err = %v", err)
			}
			got := decodeFacts(t, out)
			if len(got) != len(tc.wantFacts) {
				t.Fatalf("got %d facts, want %d: %+v", len(got), len(tc.wantFacts), got)
			}
			for i, wf := range tc.wantFacts {
				if got[i] != wf {
					t.Fatalf("fact %d = %+v, want %+v", i, got[i], wf)
				}
			}
		})
	}
}

// TestDW_1_1_NewBackend_Ensemble_WiresJudgeAsClaudeSonnet confirms
// `-backend ensemble` resolves through the production newBackend factory
// into an ensembleBackend whose Judge is claudeBackend{Model:"sonnet"} and
// whose Agy/Codex are the real leaf backends (not fakes) — the wiring half
// of DW-1.1, distinct from the hermetic behavior tested above.
func TestDW_1_1_NewBackend_Ensemble_WiresJudgeAsClaudeSonnet(t *testing.T) {
	b, err := newBackend("ensemble", "")
	if err != nil {
		t.Fatalf("newBackend(\"ensemble\") err = %v", err)
	}
	e, ok := b.(ensembleBackend)
	if !ok {
		t.Fatalf("newBackend(\"ensemble\") = %T, want ensembleBackend", b)
	}
	judge, ok := e.Judge.(claudeBackend)
	if !ok {
		t.Fatalf("ensembleBackend.Judge = %T, want claudeBackend", e.Judge)
	}
	if judge.Model != "sonnet" {
		t.Fatalf("judge model = %q, want %q", judge.Model, "sonnet")
	}
	if _, ok := e.Agy.(agyBackend); !ok {
		t.Fatalf("ensembleBackend.Agy = %T, want agyBackend", e.Agy)
	}
	if _, ok := e.Codex.(codexBackend); !ok {
		t.Fatalf("ensembleBackend.Codex = %T, want codexBackend", e.Codex)
	}
}

// TestNewBackend_Ensemble_ModelFlagAppliesToCandidatesNotJudge confirms the
// -model override threads to Agy/Codex but never to the judge, which stays
// pinned to judgeModel ("sonnet") — an internal constant per the plan, not a
// new flag.
func TestNewBackend_Ensemble_ModelFlagAppliesToCandidatesNotJudge(t *testing.T) {
	b, err := newBackend("ensemble", "custom-model")
	if err != nil {
		t.Fatalf("newBackend err = %v", err)
	}
	e := b.(ensembleBackend)
	if got := e.Agy.(agyBackend).Model; got != "custom-model" {
		t.Fatalf("Agy.Model = %q, want %q", got, "custom-model")
	}
	if got := e.Codex.(codexBackend).Model; got != "custom-model" {
		t.Fatalf("Codex.Model = %q, want %q", got, "custom-model")
	}
	if got := e.Judge.(claudeBackend).Model; got != judgeModel {
		t.Fatalf("Judge.Model = %q, want unaffected %q", got, judgeModel)
	}
}

// TestDW_1_2_JudgeArgvIsClaudeSonnetWithStrictSystemPrompt is the structural
// (no live call) half of DW-1.2: the judge's own argv-builder, given
// judgeSystemPrompt and an assembled user content, produces
// `claude --model sonnet --system-prompt <strict prompt> -p <content>`-shaped
// args; the system prompt explicitly forbids CLAUDE.md/config leakage.
func TestDW_1_2_JudgeArgvIsClaudeSonnetWithStrictSystemPrompt(t *testing.T) {
	userContent := assembleJudgeUserContent("source text", candidateResult{source: "agy"}, candidateResult{source: "codex"})
	args := claudeBackend{Model: judgeModel}.args(judgeSystemPrompt, userContent)

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--model") || !strings.Contains(joined, judgeModel) {
		t.Fatalf("judge argv = %v, want --model %s", args, judgeModel)
	}
	if !strings.Contains(joined, "--system-prompt") {
		t.Fatalf("judge argv = %v, want --system-prompt", args)
	}
	found := false
	for _, a := range args {
		if a == judgeSystemPrompt {
			found = true
		}
	}
	if !found {
		t.Fatalf("judge argv %v does not carry judgeSystemPrompt as a whole argv element", args)
	}
	lower := strings.ToLower(judgeSystemPrompt)
	if !strings.Contains(lower, "claude.md") && !strings.Contains(lower, "configuration") {
		t.Fatalf("judgeSystemPrompt does not explicitly guard against CLAUDE.md/config leakage: %s", judgeSystemPrompt)
	}
}

// TestDW_1_2_AssembleJudgeUserContent_ContainsSourceAndBothCandidateSets
// proves the assembled user content contains the source event and both
// candidate sets, clearly delimited (distinguishable from one another).
func TestDW_1_2_AssembleJudgeUserContent_ContainsSourceAndBothCandidateSets(t *testing.T) {
	agy := candidateResult{source: "agy", facts: []fact{{Subject: "rtd", Predicate: "prefers", Object: "tabs"}}}
	codex := candidateResult{source: "codex", facts: []fact{{Subject: "rtd", Predicate: "uses", Object: "zsh"}}}
	got := assembleJudgeUserContent("rtd prefers tabs and uses zsh", agy, codex)

	for _, want := range []string{"rtd prefers tabs and uses zsh", "prefers", "tabs", "uses", "zsh", "CANDIDATE A", "CANDIDATE B", "SOURCE EVENT"} {
		if !strings.Contains(got, want) {
			t.Fatalf("assembled judge content missing %q; got:\n%s", want, got)
		}
	}
	// The two candidate sets must be distinguishable, not merged into one
	// indistinguishable blob.
	idxA := strings.Index(got, "CANDIDATE A")
	idxB := strings.Index(got, "CANDIDATE B")
	if idxA < 0 || idxB < 0 || idxA >= idxB {
		t.Fatalf("CANDIDATE A must be clearly delimited before CANDIDATE B; got:\n%s", got)
	}
}

// TestDW_1_3a_OneExtractorFails_JudgeRunsOnSurvivor covers DW-1.3(a): if one
// extractor fails, the judge still runs — on the surviving candidate set,
// with the failed leg disclosed rather than silently omitted.
func TestDW_1_3a_OneExtractorFails_JudgeRunsOnSurvivor(t *testing.T) {
	var judgeSawUser string
	judgeCalled := false
	judge := capturingBackend{raw: `[{"subject":"rtd","predicate":"uses","object":"zsh"}]`, gotUser: &judgeSawUser}
	e := ensembleBackend{
		Agy:   fakeCandidate("", errors.New("agy: exit 1")),
		Codex: fakeCandidate(`[{"subject":"rtd","predicate":"uses","object":"zsh"}]`, nil),
		Judge: judgeWrapper{inner: judge, called: &judgeCalled},
	}
	out, err := e.Run(context.Background(), "sys", "rtd uses zsh")
	if err != nil {
		t.Fatalf("Run() err = %v, want the judge to run on the codex survivor", err)
	}
	if !judgeCalled {
		t.Fatal("judge was never invoked despite one surviving candidate set")
	}
	if !strings.Contains(judgeSawUser, "zsh") {
		t.Fatalf("judge did not see the surviving codex candidate set: %s", judgeSawUser)
	}
	if !strings.Contains(strings.ToLower(judgeSawUser), "unavailable") {
		t.Fatalf("judge was not told agy's leg was unavailable: %s", judgeSawUser)
	}
	got := decodeFacts(t, out)
	if len(got) != 1 || got[0].Object != "zsh" {
		t.Fatalf("Run() facts = %+v, want the zsh fact", got)
	}
}

// judgeWrapper records whether the judge was invoked at all, wrapping any
// Backend.
type judgeWrapper struct {
	inner  Backend
	called *bool
}

func (j judgeWrapper) Run(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	*j.called = true
	return j.inner.Run(ctx, systemPrompt, userPrompt)
}

// TestDW_1_3b_BothExtractorsFail_RetryableErrorNoHang covers DW-1.3(b): when
// both agy and codex fail, Run returns promptly with an error wrapping
// ErrBackendUnavailable (the HTTP layer's existing 502/retryable mapping) —
// never a hang, and the judge is never invoked over nothing.
func TestDW_1_3b_BothExtractorsFail_RetryableErrorNoHang(t *testing.T) {
	judgeCalled := false
	e := ensembleBackend{
		Agy:   fakeCandidate("", errors.New("agy: exit 1")),
		Codex: fakeCandidate("", errors.New("codex: exit 1")),
		Judge: judgeWrapper{inner: fakeCandidate(`[]`, nil), called: &judgeCalled},
	}

	done := make(chan error, 1)
	go func() {
		_, err := e.Run(context.Background(), "sys", "user")
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrBackendUnavailable) {
			t.Fatalf("Run() err = %v, want wrapping ErrBackendUnavailable", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return within 2s when both extractors failed — it hung")
	}
	if judgeCalled {
		t.Fatal("judge was invoked despite both candidate extractors failing — nothing to reconcile")
	}
}

// TestDW_1_3c_JudgeFails_FallsBackToAgySetDeduped covers the first half of
// DW-1.3(c): a judge error falls back to agy's candidate set, deduped.
func TestDW_1_3c_JudgeFails_FallsBackToAgySetDeduped(t *testing.T) {
	e := ensembleBackend{
		Agy:   fakeCandidate(`[{"subject":"rtd","predicate":"prefers","object":"tabs"},{"subject":"rtd","predicate":"prefers","object":"tabs"}]`, nil),
		Codex: fakeCandidate(`[{"subject":"rtd","predicate":"uses","object":"zsh"}]`, nil),
		Judge: fakeCandidate("", errors.New("claude: exit 1")),
	}
	out, err := e.Run(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("Run() err = %v, want fallback (no error)", err)
	}
	got := decodeFacts(t, out)
	if len(got) != 1 || got[0].Object != "tabs" {
		t.Fatalf("Run() facts = %+v, want agy's single deduped tabs fact, not codex's", got)
	}
}

// TestDW_1_3c_JudgeReturnsGarbage_FallsBackToAgySetDeduped covers the second
// half of DW-1.3(c): the judge returning unparseable garbage (not a
// legitimate "[]") also triggers the agy fallback, distinct from a judge
// that legitimately decided nothing survives.
func TestDW_1_3c_JudgeReturnsGarbage_FallsBackToAgySetDeduped(t *testing.T) {
	e := ensembleBackend{
		Agy:   fakeCandidate(`[{"subject":"rtd","predicate":"prefers","object":"tabs"}]`, nil),
		Codex: fakeCandidate(`[]`, nil),
		Judge: fakeCandidate("I couldn't find any facts, sorry!", nil), // no brackets at all -> garbage
	}
	out, err := e.Run(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("Run() err = %v, want fallback (no error)", err)
	}
	got := decodeFacts(t, out)
	if len(got) != 1 || got[0].Object != "tabs" {
		t.Fatalf("Run() facts = %+v, want agy's tabs fact via garbage fallback", got)
	}
}

// TestDW_1_3c_JudgeLegitimateEmpty_NotTreatedAsGarbage is the negative case:
// a judge that returns a well-formed "[]" (a real reconciliation decision)
// must NOT trigger the agy fallback — that would defeat the judge's purpose.
func TestDW_1_3c_JudgeLegitimateEmpty_NotTreatedAsGarbage(t *testing.T) {
	e := ensembleBackend{
		Agy:   fakeCandidate(`[{"subject":"rtd","predicate":"maybe","object":"unclear"}]`, nil),
		Codex: fakeCandidate(`[]`, nil),
		Judge: fakeCandidate(`[]`, nil),
	}
	out, err := e.Run(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	got := decodeFacts(t, out)
	if len(got) != 0 {
		t.Fatalf("Run() facts = %+v, want the judge's legitimate empty decision respected (no fallback to agy)", got)
	}
}

// TestDW_1_3c_JudgeFailsAndAgyFailed_FallsBackToCodexSet covers the
// combination where the leg the judge would normally fall back to (agy) is
// itself the one that failed — the fallback must degrade further to
// codex's surviving set rather than losing everything.
func TestDW_1_3c_JudgeFailsAndAgyFailed_FallsBackToCodexSet(t *testing.T) {
	e := ensembleBackend{
		Agy:   fakeCandidate("", errors.New("agy: exit 1")),
		Codex: fakeCandidate(`[{"subject":"rtd","predicate":"uses","object":"zsh"}]`, nil),
		Judge: fakeCandidate("", errors.New("claude: exit 1")),
	}
	out, err := e.Run(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("Run() err = %v, want fallback to codex (no error)", err)
	}
	got := decodeFacts(t, out)
	if len(got) != 1 || got[0].Object != "zsh" {
		t.Fatalf("Run() facts = %+v, want codex's zsh fact", got)
	}
}

// sleepingBackend blocks until ctx is done or delay elapses, recording when
// it started and finished — used to prove concurrency (DW-1.4).
type sleepingBackend struct {
	delay      time.Duration
	raw        string
	start, end *time.Time
}

func (s sleepingBackend) Run(ctx context.Context, _, _ string) (string, error) {
	now := time.Now()
	*s.start = now
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		*s.end = time.Now()
		return "", ctx.Err()
	}
	*s.end = time.Now()
	return s.raw, nil
}

// TestDW_1_4_AgyAndCodexRunConcurrently_NotSerially proves agy and codex are
// invoked concurrently: two 150ms-sleeping fakes must overlap in wall-clock
// time, and the whole runCandidates call must take roughly one delay's
// worth of time, not the sum of both.
func TestDW_1_4_AgyAndCodexRunConcurrently_NotSerially(t *testing.T) {
	var agyStart, agyEnd, codexStart, codexEnd time.Time
	delay := 150 * time.Millisecond
	e := ensembleBackend{
		Agy:   sleepingBackend{delay: delay, raw: "[]", start: &agyStart, end: &agyEnd},
		Codex: sleepingBackend{delay: delay, raw: "[]", start: &codexStart, end: &codexEnd},
		Judge: fakeCandidate("[]", nil),
	}

	started := time.Now()
	e.runCandidates(context.Background(), "sys", "user")
	elapsed := time.Since(started)

	if elapsed > delay+100*time.Millisecond {
		t.Fatalf("runCandidates took %v for two %v-sleeping legs, want ~%v (concurrent), not ~%v (serial)", elapsed, delay, delay, 2*delay)
	}
	// Overlap check: agy must start before codex finishes, and vice versa.
	if !agyStart.Before(codexEnd) || !codexStart.Before(agyEnd) {
		t.Fatalf("legs did not overlap: agy [%v,%v] codex [%v,%v]", agyStart, agyEnd, codexStart, codexEnd)
	}
}

// TestDW_1_4_EnsembleHonorsPerCallTimeout_NoHang proves the whole ensemble
// (agy ∥ codex, then judge) is bounded by the caller's context deadline —
// a slow stage cannot hang the request past that deadline, mirroring the
// existing WaitDelay-backstop guarantee runProcess already provides per-leg.
func TestDW_1_4_EnsembleHonorsPerCallTimeout_NoHang(t *testing.T) {
	slow := fakeBackend{run: func(ctx context.Context, _, _ string) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
			return "[]", nil
		}
	}}
	e := ensembleBackend{Agy: slow, Codex: slow, Judge: slow}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := e.Run(ctx, "sys", "user")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run() succeeded despite every leg blocking past the context deadline")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return within 2s of a 50ms context deadline — it hung")
	}
}

// TestDW_1_6_DirtyTextReachesAllThreeBackendsInert is the ensemble-level
// dirty test: shell-metacharacter-laden source event text reaches agy and
// codex byte-for-byte as their userPrompt, and reaches the judge as an
// intact substring of the assembled prompt, which — when handed to the
// REAL claudeBackend.args() builder — still lands as a single opaque argv
// element (never split, never shell-interpreted), reusing the existing
// backend_test.go assertion helper.
func TestDW_1_6_DirtyTextReachesAllThreeBackendsInert(t *testing.T) {
	var agySawUser, codexSawUser, judgeSawUser string
	e := ensembleBackend{
		Agy:   capturingBackend{raw: "[]", gotUser: &agySawUser},
		Codex: capturingBackend{raw: "[]", gotUser: &codexSawUser},
		Judge: capturingBackend{raw: "[]", gotUser: &judgeSawUser},
	}

	_, err := e.Run(context.Background(), "sys", dirtyEventText)
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}

	if agySawUser != dirtyEventText {
		t.Fatalf("agy received userPrompt = %q, want exactly the dirty text unchanged", agySawUser)
	}
	if codexSawUser != dirtyEventText {
		t.Fatalf("codex received userPrompt = %q, want exactly the dirty text unchanged", codexSawUser)
	}
	if !strings.Contains(judgeSawUser, dirtyEventText) {
		t.Fatalf("judge's assembled prompt does not contain the dirty text intact: %s", judgeSawUser)
	}

	// Re-verify through the real claude argv builder: the assembled prompt
	// (carrying the dirty text as a substring) must still land as exactly
	// one opaque argv element when built the way the real judge would.
	args := claudeBackend{Model: judgeModel}.args(judgeSystemPrompt, judgeSawUser)
	assertOpaqueSubstringInOneArg(t, args, dirtyEventText)
}

// TestDedupeFacts confirms dedup drops exact and whitespace-only duplicates
// while preserving first-occurrence order and keeping distinct facts.
func TestDedupeFacts(t *testing.T) {
	in := []fact{
		{Subject: "rtd", Predicate: "prefers", Object: "tabs"},
		{Subject: "rtd", Predicate: "prefers", Object: "tabs"},
		{Subject: " rtd ", Predicate: "prefers", Object: "tabs "},
		{Subject: "rtd", Predicate: "uses", Object: "zsh"},
	}
	got := dedupeFacts(in)
	if len(got) != 2 {
		t.Fatalf("dedupeFacts(%+v) = %+v, want 2 distinct facts", in, got)
	}
	if got[0].Object != "tabs" || got[1].Object != "zsh" {
		t.Fatalf("dedupeFacts did not preserve first-occurrence order: %+v", got)
	}
}

// TestParseJudgeFacts_DistinguishesGarbageFromLegitimateEmpty is the unit
// test for the helper DW-1.3(c)'s fallback decision depends on.
func TestParseJudgeFacts_DistinguishesGarbageFromLegitimateEmpty(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantOK  bool
		wantLen int
	}{
		{name: "legitimate empty array", raw: "[]", wantOK: true, wantLen: 0},
		{name: "well-formed single fact", raw: `[{"subject":"rtd","predicate":"uses","object":"zsh"}]`, wantOK: true, wantLen: 1},
		{name: "fenced well-formed", raw: "```json\n[]\n```", wantOK: true, wantLen: 0},
		{name: "no brackets at all", raw: "I found nothing.", wantOK: false},
		{name: "empty stdout", raw: "", wantOK: false},
		{name: "array of non-objects", raw: `["not","an","object"]`, wantOK: false},
		{name: "invalid json", raw: "[{not json", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			facts, ok := parseJudgeFacts(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("parseJudgeFacts(%q) ok = %v, want %v", tc.raw, ok, tc.wantOK)
			}
			if tc.wantOK && len(facts) != tc.wantLen {
				t.Fatalf("parseJudgeFacts(%q) len = %d, want %d", tc.raw, len(facts), tc.wantLen)
			}
		})
	}
}
