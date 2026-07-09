package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// judgeModel pins the judge to claude-sonnet-5 (`claude --model sonnet`) —
// an internal, documented constant per the plan's explicit instruction that
// the judge's model is not a new flag.
const judgeModel = "sonnet"

// judgeSystemPrompt is the judge's strict system prompt (DW-1.2). It exists
// to make the judge robust to claude's global-CLAUDE.md injection
// (~24k tokens on every `claude` call, per
// .code-foundations/research/2026-07-08-extraction-cli-shim.md): the judge's
// ONLY job is reconciling the two given candidate sets against the given
// source event, and it is explicitly told never to invent facts about its
// own configuration, instructions, environment, or any global/system config
// file. DW-1.5's live guard test proves this holds against the real CLI.
const judgeSystemPrompt = `You are a strict fact-reconciliation judge for a personal memory system.

You will be given exactly one SOURCE EVENT and two CANDIDATE fact-set arrays (CANDIDATE A and CANDIDATE B), each already extracted from that SAME source event by a different extraction model. One candidate set may be marked unavailable if its extractor failed.

Your ONLY job: reconcile CANDIDATE A and CANDIDATE B against the SOURCE EVENT text and return ONE authoritative JSON array of facts.

Rules:
- Every fact you return MUST be directly and verifiably supported by the SOURCE EVENT text given below. Do not include a fact that is not stated or clearly implied by the SOURCE EVENT.
- Do NOT invent, infer, or report any fact about anything other than the SOURCE EVENT — never a fact about your own configuration, instructions, tools, environment, or any system/global configuration file (including anything named CLAUDE.md). Those are not part of the SOURCE EVENT and must never appear in your output.
- Merge duplicate or near-duplicate facts from CANDIDATE A and CANDIDATE B into one fact.
- Drop any candidate fact that is not supported by the SOURCE EVENT.
- Return ONLY a JSON array (no prose, no code fences) of objects: {"subject": string, "predicate": string, "object": string, "statement": string, "valid_at": RFC3339 timestamp or omit}.
- If neither candidate set contains any fact supported by the SOURCE EVENT, return [].`

// candidateResult is one extractor leg's outcome (agy or codex): its parsed,
// canonical facts on success, or the error that made it unavailable.
type candidateResult struct {
	source string // "agy" or "codex" — used for judge-prompt labeling and fallback preference
	facts  []fact
	err    error
}

// ensembleBackend is a Composite (GoF) over the existing Backend Strategy
// interface: it composes two leaf extractors (Agy, Codex — run
// concurrently) and a judge (Judge — a claude-sonnet-5 reconciler) into one
// Backend, so callers (server.go, newBackend) never need to know whether
// they're holding a leaf or a composite. Fields are exported and
// Backend-typed so tests inject fakeBackend for all three legs without a new
// fake type.
type ensembleBackend struct {
	Agy   Backend
	Codex Backend
	Judge Backend
}

// Run implements Backend: fan out to Agy and Codex concurrently, reconcile
// their candidate sets with Judge, and return the judge's canonical JSON
// fact array — falling back per the degrade-never-crash rules in
// ensembleBackend's design (see the Phase 1 discovery doc) rather than ever
// hanging or returning a bare 500.
func (e ensembleBackend) Run(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	agyResult, codexResult := e.runCandidates(ctx, systemPrompt, userPrompt)

	// DW-1.3(b): both extractors failed — nothing to reconcile. Signal a
	// retryable failure (consistent with every leaf backend's own
	// failure-signaling convention) rather than silently returning "[]",
	// which would stamp processed_at and permanently lose the event's facts.
	if agyResult.err != nil && codexResult.err != nil {
		return "", fmt.Errorf("%w: ensemble: both agy and codex candidate extractors failed: agy: %v; codex: %v", ErrBackendUnavailable, agyResult.err, codexResult.err)
	}

	judgeUserContent := assembleJudgeUserContent(userPrompt, agyResult, codexResult)
	judgeRaw, judgeErr := e.Judge.Run(ctx, judgeSystemPrompt, judgeUserContent)
	if judgeErr == nil {
		if facts, ok := parseJudgeFacts(judgeRaw); ok {
			return marshalFacts(facts), nil
		}
	}

	// DW-1.3(c): the judge failed, timed out, or returned garbage — fall
	// back to agy's candidate set (the plan's literal fallback choice),
	// or codex's if agy itself was the leg that failed, deduped.
	fallback := agyResult
	if fallback.err != nil {
		fallback = codexResult
	}
	return marshalFacts(dedupeFacts(fallback.facts)), nil
}

// runCandidates invokes Agy and Codex concurrently against the same ctx and
// returns both results once both have completed (DW-1.4). A fixed two-way
// fan-out — a sync.WaitGroup over two goroutines is simpler than adding
// golang.org/x/sync/errgroup as a new module dependency for a bounded,
// non-dynamic join.
func (e ensembleBackend) runCandidates(ctx context.Context, systemPrompt, userPrompt string) (agyResult, codexResult candidateResult) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		agyResult = runCandidate(ctx, "agy", e.Agy, systemPrompt, userPrompt)
	}()
	go func() {
		defer wg.Done()
		codexResult = runCandidate(ctx, "codex", e.Codex, systemPrompt, userPrompt)
	}()
	wg.Wait()
	return agyResult, codexResult
}

// runCandidate invokes one leaf backend and normalizes its raw stdout into a
// candidateResult via the existing parseFacts barricade — the judge always
// sees clean canonical JSON, never raw CLI banner/prose noise.
func runCandidate(ctx context.Context, source string, backend Backend, systemPrompt, userPrompt string) candidateResult {
	raw, err := backend.Run(ctx, systemPrompt, userPrompt)
	if err != nil {
		return candidateResult{source: source, err: err}
	}
	canonical, _ := parseFacts(raw)
	var facts []fact
	_ = json.Unmarshal(canonical, &facts) // parseFacts guarantees valid []fact-shaped JSON
	return candidateResult{source: source, facts: facts}
}

// assembleJudgeUserContent builds the judge's user-content string: the
// source event text plus both candidate sets, clearly delimited. Pure and
// side-effect-free so DW-1.2 can assert its shape without a live call. Dirty
// text is always substituted via a %s argument, never used as the format
// template (format-string-injection discipline).
func assembleJudgeUserContent(sourceEvent string, agy, codex candidateResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "SOURCE EVENT:\n%s\n\n", sourceEvent)
	fmt.Fprintf(&b, "%s\n\n", candidateBlock("CANDIDATE A (agy)", agy))
	fmt.Fprintf(&b, "%s\n", candidateBlock("CANDIDATE B (codex)", codex))
	return b.String()
}

// candidateBlock renders one leg's result for the judge prompt: its
// canonical JSON fact array on success, or an explicit "unavailable" note on
// failure — so the judge (and a test) can see a failed leg was disclosed,
// never silently omitted.
func candidateBlock(label string, r candidateResult) string {
	if r.err != nil {
		return fmt.Sprintf("%s: unavailable (extractor failed: %s)", label, r.err)
	}
	return fmt.Sprintf("%s:\n%s", label, marshalFacts(r.facts))
}

// parseJudgeFacts parses the judge's raw output more strictly than
// parseFacts: it distinguishes a legitimate, well-formed empty array ("[]" —
// the judge decided nothing survives reconciliation, accepted as-is) from
// garbage (no array found, or the array doesn't unmarshal into []fact —
// triggers the DW-1.3(c) fallback). parseFacts alone can't make this
// distinction because it collapses both cases to "[]".
func parseJudgeFacts(raw string) (facts []fact, ok bool) {
	stripped := stripCodeFences(raw)
	arr := extractArraySubstring(stripped)
	if arr == "" {
		return nil, false
	}
	if err := json.Unmarshal([]byte(arr), &facts); err != nil {
		return nil, false
	}
	return facts, true
}

// dedupeFacts drops duplicate facts (same subject/predicate/object, compared
// trimmed) from the fallback candidate set, preserving first-occurrence
// order.
func dedupeFacts(facts []fact) []fact {
	seen := make(map[string]struct{}, len(facts))
	out := make([]fact, 0, len(facts))
	for _, f := range facts {
		key := strings.TrimSpace(f.Subject) + "\x00" + strings.TrimSpace(f.Predicate) + "\x00" + strings.TrimSpace(f.Object)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, f)
	}
	return out
}

// marshalFacts is a small helper wrapping json.Marshal for []fact with a
// degrade-to-empty-array fallback on the (practically unreachable) marshal
// error case, so ensembleBackend.Run never returns malformed JSON.
func marshalFacts(facts []fact) string {
	if facts == nil {
		facts = []fact{}
	}
	out, err := json.Marshal(facts)
	if err != nil {
		return emptyFactArray
	}
	return string(out)
}
