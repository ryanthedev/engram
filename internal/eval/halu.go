package eval

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/ryanthedev/engram/internal/retrieval"
)

// This file is the HaluMem-style hallucination suite (D9/Phase 8, DW-8.1): it
// measures facts a memory system asserts that its own known-good fixture
// corpus does not support. It is deliberately independent of the retrieval
// regression gate (harness.go) and the experience-following gate
// (internal/eval/gate) — Phase 8's constraint is combining three
// independently-failing detectors, not building one bigger one.

// HaluCase is one HaluMem-style probe: a query issued against the memory
// system, graded against the fixed corpus every case shares (HaluGoldSet.Corpus)
// rather than a per-case document, since a hallucinated assertion may not
// correspond to any single source doc at all.
type HaluCase struct {
	ID    string
	Query string
}

// HaluGoldSet is the checked-in hallucination fixture: a known-good corpus
// (the ground truth a correct memory must stay within) and the probe queries
// run against it.
type HaluGoldSet struct {
	Corpus []Doc
	Cases  []HaluCase
}

// HaluJudge decides whether a memory-asserted statement is grounded in
// (supported by) the known-good corpus, or a hallucination. RuleHaluJudge is
// the deterministic fixture judge (tests, local gate runs); HTTPHaluJudge is
// the production LLM judge behind the identical interface — the same
// Rule/HTTP duality internal/experience already established for its
// write-gate judge.
type HaluJudge interface {
	// Supported reports whether statement is grounded in corpus. An error
	// means the judge could not decide; callers must treat that as NOT
	// supported (fail-closed) rather than silently passing unverified
	// content — RunHallucinationSuite does exactly that.
	Supported(ctx context.Context, corpus, statement string) (bool, error)
}

// RuleTokenOverlapThreshold is the fraction of a statement's meaningful
// tokens that must appear in the corpus for RuleHaluJudge to call it
// supported. Exactly at the threshold is supported (a "≥" contract, tested
// at the boundary) — consistent with the regression gate's non-inferiority
// contract shape.
const RuleTokenOverlapThreshold = 0.5

// RuleHaluJudge is the deterministic, fixture-driven HaluJudge: it never
// errors and never calls out to a model, so the suite's pass/fail behavior is
// reproducible in unit tests and local gate runs — the same role
// RuleGatekeeper/RuleExtractor play elsewhere. It approximates grounding by
// token overlap: a statement is supported when at least
// RuleTokenOverlapThreshold of its meaningful (len>=3) tokens appear
// somewhere in the corpus text. This is intentionally coarse — it is a
// stand-in for an LLM's entailment judgment, not a replacement for one.
type RuleHaluJudge struct{}

var _ HaluJudge = RuleHaluJudge{}

var tokenRe = regexp.MustCompile(`[a-z0-9]+`)

// Supported implements HaluJudge. It never returns an error.
func (RuleHaluJudge) Supported(_ context.Context, corpus, statement string) (bool, error) {
	corpusTokens := tokenSet(corpus)
	stmtTokens := meaningfulTokens(statement)
	if len(stmtTokens) == 0 {
		// An empty/degenerate statement asserts nothing checkable; treat as
		// unsupported rather than vacuously true.
		return false, nil
	}
	matched := 0
	for _, t := range stmtTokens {
		if corpusTokens[t] {
			matched++
		}
	}
	frac := float64(matched) / float64(len(stmtTokens))
	return frac >= RuleTokenOverlapThreshold, nil
}

func meaningfulTokens(s string) []string {
	var out []string
	for _, t := range tokenRe.FindAllString(strings.ToLower(s), -1) {
		if len(t) >= 3 {
			out = append(out, t)
		}
	}
	return out
}

func tokenSet(s string) map[string]bool {
	set := make(map[string]bool)
	for _, t := range meaningfulTokens(s) {
		set[t] = true
	}
	return set
}

// HaluCaseResult is one case's scored outcome: how many statements the
// memory asserted in response to the probe query, and which of them the
// judge flagged as unsupported.
type HaluCaseResult struct {
	CaseID       string
	Total        int
	Unsupported  []string
	JudgeErrored int
}

// HaluReport aggregates a hallucination-suite run: Rate is the hallucination
// rate — Hallucinated / Assertions — the metric DW-8.1's gate thresholds
// against. Zero assertions across the whole run reports Rate 0 (nothing
// asserted, nothing to hallucinate), not a divide-by-zero.
type HaluReport struct {
	Cases        int
	Assertions   int
	Hallucinated int
	Rate         float64
	Details      []HaluCaseResult
}

// RunHallucinationSuite runs every case's Query against r at cutoff k, reads
// each hit's "statement" field (the fact text a real engramd indexes — see
// internal/eval/seed and internal/ingest's synthesizeStatement), and judges
// it against the gold set's whole corpus text. A hit with no "statement"
// field is skipped (not every tier's hits carry one; episodic hits use
// "text" and are out of scope for a *fact*-hallucination measure). A
// per-query retrieval error is recorded (JudgeErrored via a synthetic empty
// result), not fatal. filter scopes every query (e.g. TenantID, ValidOnly) —
// on a shared cluster this is what keeps the suite from picking up other
// tenants'/scenarios' facts; pass retrieval.Filter{ValidOnly: true} when the
// target index is already exclusively the suite's own (a dedicated scratch
// index).
func RunHallucinationSuite(ctx context.Context, r retrieval.Retriever, gs HaluGoldSet, judge HaluJudge, k int, filter retrieval.Filter) (HaluReport, error) {
	if k <= 0 {
		return HaluReport{}, fmt.Errorf("eval: k must be positive, got %d", k)
	}
	if judge == nil {
		return HaluReport{}, fmt.Errorf("eval: judge must not be nil")
	}
	corpus := joinCorpus(gs.Corpus)
	var rep HaluReport
	rep.Cases = len(gs.Cases)
	for _, c := range gs.Cases {
		cr := HaluCaseResult{CaseID: c.ID}
		hits, err := r.Search(ctx, retrieval.Query{Text: c.Query, K: k}, filter)
		if err != nil {
			rep.Details = append(rep.Details, cr)
			continue
		}
		for _, h := range hits {
			stmt, _ := h.Fields["statement"].(string)
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			cr.Total++
			rep.Assertions++
			ok, jerr := judge.Supported(ctx, corpus, stmt)
			if jerr != nil {
				cr.JudgeErrored++
				ok = false // fail-closed: a judge error counts as a hallucination
			}
			if !ok {
				cr.Unsupported = append(cr.Unsupported, stmt)
				rep.Hallucinated++
			}
		}
		rep.Details = append(rep.Details, cr)
	}
	if rep.Assertions > 0 {
		rep.Rate = float64(rep.Hallucinated) / float64(rep.Assertions)
	}
	return rep, nil
}

func joinCorpus(docs []Doc) string {
	parts := make([]string, len(docs))
	for i, d := range docs {
		parts[i] = d.Text
	}
	return strings.Join(parts, "\n")
}

// HaluFixtures returns the checked-in-by-construction hallucination fixture:
// a small known-good corpus about a fictional service (independent of the
// DW-1.3 goldset's Meridian org, so the two gates never share ground truth)
// and one probe query per corpus fact. A correct memory system, given only
// this corpus, asserts facts entirely grounded in it — RunHallucinationSuite
// against a faithful extractor reports Rate 0 on these cases.
func HaluFixtures() HaluGoldSet {
	facts := []struct{ id, text, query string }{
		{"orbitctl-timeout", "orbitctl sets a hard rpc timeout of 4000ms for the scheduler service.", "what is orbitctl's rpc timeout"},
		{"orbitctl-owner", "The platform-runtime team owns orbitctl; escalation goes to @sana.iyer.", "who owns orbitctl"},
		{"orbitctl-region", "orbitctl's primary control plane runs in the ap-southeast-2 region.", "where does orbitctl's control plane run"},
		{"orbitctl-quota", "Each namespace in orbitctl gets a default quota of 200 scheduled jobs per day.", "what is the default orbitctl job quota"},
		{"orbitctl-retry", "orbitctl retries a failed job 3 times with a 30 second backoff before marking it dead.", "how many times does orbitctl retry a failed job"},
		{"orbitctl-auth", "orbitctl authenticates internal callers via mtls certificates issued by cert-minter.", "how does orbitctl authenticate internal callers"},
		{"orbitctl-storage", "orbitctl persists job state in a dedicated postgres instance named orbitctl-state-01.", "where does orbitctl store job state"},
		{"orbitctl-alert", "orbitctl pages on-call when scheduler queue depth exceeds 5000 pending jobs.", "when does orbitctl page on call"},
	}
	gs := HaluGoldSet{}
	for _, f := range facts {
		gs.Corpus = append(gs.Corpus, Doc{ID: f.id, Text: f.text})
		gs.Cases = append(gs.Cases, HaluCase{ID: f.id, Query: f.query})
	}
	return gs
}
