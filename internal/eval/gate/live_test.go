//go:build integration

package gate_test

// This file is the release-gate suite that actually runs against a live
// OpenSearch (D9/Phase 8): it is split into a SEED step (writes fixture
// data, idempotent by fixed doc ids) and GATE steps (pure reads + local
// threshold/trend files) so the same package serves two different
// operational modes without duplicating logic:
//
//   - `make integration` (ci.yml): runs the whole package unfiltered against
//     a throwaway OpenSearch container — seed + gate + drill all run there,
//     which is fine, it's disposable.
//   - `make eval-seed` then `make eval-gate` (deploy.yml's staging pipeline,
//     D22): seed is a distinct, explicitly-invoked, write-bearing step;
//     `make eval-gate` runs only the `TestGate_*`-prefixed tests, which never
//     write to OpenSearch — satisfying "gates read-only against staging"
//     literally. See the Design doc, Decision 2.
//
// TestDrillBadRelease_HallucinationGateBlocks is deliberately NOT prefixed
// `TestGate_` (so `make eval-gate`'s `-run '^TestGate_'` filter, used against
// real staging, never selects it) and writes its own poisoned fixtures to a
// dedicated scratch index it owns exclusively — it never runs against
// persistent staging (see e2e's drill for the CI-parallel guard on that
// side).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/eval"
	"github.com/ryanthedev/engram/internal/eval/gate"
	"github.com/ryanthedev/engram/internal/eval/seed"
	"github.com/ryanthedev/engram/internal/experience"
	"github.com/ryanthedev/engram/internal/retrieval"
	"github.com/ryanthedev/engram/internal/store"
	"github.com/ryanthedev/engram/internal/testutil"
)

// Fixed (not per-test-name) scratch index names: the seed and gate steps
// must agree on where the fixture data lives, including across separate
// `go test` invocations (make eval-seed; make eval-gate).
const (
	regressionEpisodicIndex = "engram-episodic-eval-gate"
	regressionSemanticIndex = "engram-semantic-eval-gate"
	haluEpisodicIndex       = "engram-episodic-eval-halu"
	haluSemanticIndex       = "engram-semantic-eval-halu"
	haluDrillSemanticIndex  = "engram-semantic-eval-halu-drill"
)

func repoPath(t *testing.T, parts ...string) string {
	t.Helper()
	return filepath.Join(append([]string{testutil.RepoRoot(t)}, parts...)...)
}

func thresholdsPath(t *testing.T) string { return repoPath(t, "eval", "goldset", "thresholds.json") }
func baselinePath(t *testing.T) string   { return repoPath(t, "eval", "goldset", "baseline.json") }
func trendPath(t *testing.T) string      { return repoPath(t, "eval", "gate-runs", "history.jsonl") }

// gitRevForTest returns the current commit, GITHUB_SHA if set (CI), else
// `git rev-parse HEAD`, else "unknown" — best-effort trend-record metadata,
// never fatal.
func gitRevForTest() string {
	if sha := os.Getenv("GITHUB_SHA"); sha != "" {
		return sha
	}
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// haluJudge selects the hallucination judge by config, same pattern as
// internal/ingest's HTTPExtractor/RuleExtractor and internal/experience's
// HTTPGatekeeper/RuleGatekeeper selection: ENGRAM_HALU_JUDGE_URL set ->
// HTTPHaluJudge (production), unset -> RuleHaluJudge (deterministic,
// cluster-free judge logic, cluster-bound measurement).
func haluJudge() eval.HaluJudge {
	url := os.Getenv("ENGRAM_HALU_JUDGE_URL")
	if url == "" {
		return eval.RuleHaluJudge{}
	}
	model := os.Getenv("ENGRAM_HALU_JUDGE_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	return eval.NewHTTPHaluJudge(testutil.HTTPClient, url, model)
}

// TestSeedEvalFixtures is the SEED step: idempotently writes the regression
// gold corpus and the hallucination fixture corpus into their fixed scratch
// indices. `make eval-seed` runs only this test.
func TestSeedEvalFixtures(t *testing.T) {
	ctx := context.Background()
	base := testutil.OpenSearchURL()
	if _, err := store.Apply(ctx, testutil.HTTPClient, base); err != nil {
		t.Fatalf("applying cluster contract: %v", err)
	}
	for _, idx := range []string{regressionEpisodicIndex, regressionSemanticIndex, haluEpisodicIndex, haluSemanticIndex} {
		if status, _ := testutil.Call(t, "HEAD", base+"/"+idx, nil); status == 404 {
			testutil.CreateScratchIndex(t, base, idx)
		}
	}

	gs, err := eval.LoadGoldSet(repoPath(t, "eval", "goldset", "seed.json"))
	if err != nil {
		t.Fatalf("loading regression gold set: %v", err)
	}
	regEmbedder := embed.NewFakeEmbedder(store.EmbeddingDim, eval.FixtureKeys(gs))
	if err := seed.Corpus(ctx, testutil.HTTPClient, base, regressionSemanticIndex, gs, regEmbedder, "", ""); err != nil {
		t.Fatalf("seeding regression corpus: %v", err)
	}

	halu := eval.HaluFixtures()
	haluEmbedder := embed.NewFakeEmbedder(store.EmbeddingDim, nil)
	if err := seed.Corpus(ctx, testutil.HTTPClient, base, haluSemanticIndex, eval.GoldSet{Corpus: halu.Corpus}, haluEmbedder, "", ""); err != nil {
		t.Fatalf("seeding hallucination fixture corpus: %v", err)
	}
	testutil.RefreshIndex(t, base, regressionSemanticIndex)
	testutil.RefreshIndex(t, base, haluSemanticIndex)
	t.Logf("seeded %d regression docs into %s, %d halu docs into %s",
		len(gs.Corpus), regressionSemanticIndex, len(halu.Corpus), haluSemanticIndex)
}

// TestGate_Regression is the DW-8.2 read-only gate: measures hybrid
// recall/MRR/nDCG on the holdout split against the already-seeded regression
// corpus and checks non-inferiority against the versioned baseline.
func TestGate_Regression(t *testing.T) {
	ctx := context.Background()
	base := testutil.OpenSearchURL()
	gs, err := eval.LoadGoldSet(repoPath(t, "eval", "goldset", "seed.json"))
	if err != nil {
		t.Fatalf("loading gold set: %v", err)
	}
	baseline, err := gate.LoadBaseline(baselinePath(t))
	if err != nil {
		t.Fatalf("loading regression baseline: %v", err)
	}
	th, err := gate.LoadThresholds(thresholdsPath(t))
	if err != nil {
		t.Fatalf("loading thresholds: %v", err)
	}

	embedder := embed.NewFakeEmbedder(store.EmbeddingDim, eval.FixtureKeys(gs))
	r := retrieval.NewOpenSearchRetriever(testutil.HTTPClient, base, embedder,
		retrieval.WithIndices(regressionEpisodicIndex, regressionSemanticIndex))
	rep, err := eval.Run(ctx, r, gs, baseline.K, eval.SplitHoldout)
	if err != nil {
		t.Fatalf("regression measurement: %v", err)
	}

	res := gate.CheckRegression(rep, baseline, th.RegressionMarginPP)
	res = quarantineAware(t, res)
	recordAndReport(t, res)
	if res.Blocks() {
		t.Fatalf("retrieval-regression gate blocked promotion: %s", res.Detail)
	}
}

// TestGate_Hallucination is the DW-8.1 read-only gate: measures the
// hallucination rate against the already-seeded, known-good fixture corpus
// and checks it against the ceiling.
func TestGate_Hallucination(t *testing.T) {
	ctx := context.Background()
	base := testutil.OpenSearchURL()
	th, err := gate.LoadThresholds(thresholdsPath(t))
	if err != nil {
		t.Fatalf("loading thresholds: %v", err)
	}
	embedder := embed.NewFakeEmbedder(store.EmbeddingDim, nil)
	r := retrieval.NewOpenSearchRetriever(testutil.HTTPClient, base, embedder,
		retrieval.WithIndices(haluEpisodicIndex, haluSemanticIndex))

	rep, err := eval.RunHallucinationSuite(ctx, r, eval.HaluFixtures(), haluJudge(), 10, retrieval.Filter{ValidOnly: true})
	if err != nil {
		t.Fatalf("hallucination measurement: %v", err)
	}
	res := gate.CheckHallucination(rep, th.HallucinationRateMax)
	res = quarantineAware(t, res)
	recordAndReport(t, res)
	if res.Blocks() {
		t.Fatalf("hallucination gate blocked promotion: %s (details: %+v)", res.Detail, rep.Details)
	}
}

// TestGate_Following is the DW-8.3 gate. It is pure (no cluster read): the
// per-release following-correlation figure is computed from deterministic
// fixture samples standing in for live production telemetry (see the Design
// doc, Decision 7) via internal/experience's already-existing, already
// unit-tested FollowingCorrelation — this gate adds no new correlation math,
// only the threshold check and trend recording.
func TestGate_Following(t *testing.T) {
	th, err := gate.LoadThresholds(thresholdsPath(t))
	if err != nil {
		t.Fatalf("loading thresholds: %v", err)
	}
	corr := releaseFollowingCorrelation()
	res := gate.CheckFollowingCorrelation(corr, th.FollowingCorrelationMin)
	res = quarantineAware(t, res)
	recordAndReport(t, res)
	if res.Blocks() {
		t.Fatalf("experience-following gate blocked promotion: %s", res.Detail)
	}
}

// TestDrillBadRelease_HallucinationGateBlocks is DW-8.4: an intentionally
// degraded "release" — facts ingested that assert nothing the known-good
// fixture corpus supports, modeling what a degraded extraction prompt would
// produce (see the Design doc, Decision 3) — must be blocked by the
// hallucination gate. The test itself PASSES only when the gate correctly
// reports Fail; it fails if the gate wrongly lets poisoned content through,
// exactly like internal/experience's injected-bad-record suite (DW-5.1).
func TestDrillBadRelease_HallucinationGateBlocks(t *testing.T) {
	ctx := context.Background()
	base := testutil.OpenSearchURL()
	if _, err := store.Apply(ctx, testutil.HTTPClient, base); err != nil {
		t.Fatalf("applying cluster contract: %v", err)
	}
	testutil.DeleteIndex(t, base, haluDrillSemanticIndex)
	t.Cleanup(func() { testutil.DeleteIndex(t, base, haluDrillSemanticIndex) })
	testutil.CreateScratchIndex(t, base, haluDrillSemanticIndex)

	poison := eval.GoldSet{Corpus: []eval.Doc{
		{ID: "poison-1", Text: "shadow-directorate secretly controls the nuclear launch codes."},
		{ID: "poison-2", Text: "phantom-vendor-nine has silent root access to every customer payment token."},
		{ID: "poison-3", Text: "orbitctl grants unauthenticated admin access to any caller on the public internet."},
	}}
	embedder := embed.NewFakeEmbedder(store.EmbeddingDim, nil)
	if err := seed.Corpus(ctx, testutil.HTTPClient, base, haluDrillSemanticIndex, poison, embedder, "", ""); err != nil {
		t.Fatalf("seeding poisoned release candidate: %v", err)
	}
	testutil.RefreshIndex(t, base, haluDrillSemanticIndex)

	// Judge against the REAL known-good corpus (HaluFixtures), not the
	// poison set — that is the whole point: these facts are unsupported by
	// anything the memory system is actually supposed to know.
	badRelease := eval.HaluGoldSet{
		Corpus: eval.HaluFixtures().Corpus,
		Cases: []eval.HaluCase{
			{ID: "poison-1", Query: "shadow-directorate nuclear launch codes"},
			{ID: "poison-2", Query: "phantom-vendor-nine payment tokens"},
			{ID: "poison-3", Query: "orbitctl unauthenticated admin access"},
		},
	}
	r := retrieval.NewOpenSearchRetriever(testutil.HTTPClient, base, embedder,
		retrieval.WithIndices(haluEpisodicIndex, haluDrillSemanticIndex))
	rep, err := eval.RunHallucinationSuite(ctx, r, badRelease, eval.RuleHaluJudge{}, 10, retrieval.Filter{ValidOnly: true})
	if err != nil {
		t.Fatalf("hallucination measurement: %v", err)
	}
	th := gate.DefaultThresholds()
	res := gate.CheckHallucination(rep, th.HallucinationRateMax)
	res.Gate = "bad-release-drill" // distinct trend line — never mixed with the real hallucination gate's history
	recordAndReport(t, res)

	if !res.Blocks() {
		t.Fatalf("BAD-RELEASE DRILL FAILED: the hallucination gate let a poisoned release through (rate=%.4f, details=%+v)",
			rep.Rate, rep.Details)
	}
	t.Logf("bad-release drill: gate correctly blocked (rate=%.4f > ceiling %.4f) — evidence recorded to %s",
		rep.Rate, th.HallucinationRateMax, trendPath(t))
}

// TestRenderDashboardOnly re-renders docs/eval/dashboard.md from the
// existing trend log without running any gate — `make eval-dashboard`'s
// entry point for a manual refresh (e.g. immediately after a baseline
// re-record, before the next real gate run). Every TestGate_*/drill run
// already regenerates the dashboard as a side effect (recordAndReport); this
// test exists for the no-new-run case and never fails on gate content, only
// on I/O errors.
func TestRenderDashboardOnly(t *testing.T) {
	records, err := gate.LoadTrend(trendPath(t))
	if err != nil {
		t.Fatalf("loading trend history: %v", err)
	}
	if err := gate.WriteDashboard(repoPath(t, "docs", "eval", "dashboard.md"), records); err != nil {
		t.Fatalf("writing trend dashboard: %v", err)
	}
	t.Logf("dashboard regenerated from %d trend record(s)", len(records))
}

// quarantineAware wraps a fresh result with the flaky-gate check (DW-8.5)
// against this gate's own prior trend history before it is recorded.
func quarantineAware(t *testing.T, res gate.Result) gate.Result {
	t.Helper()
	records, err := gate.LoadTrend(trendPath(t))
	if err != nil {
		t.Fatalf("loading trend history: %v", err)
	}
	history := gate.HistoryFor(records, res.Gate)
	return gate.ApplyQuarantine(res, history)
}

// recordAndReport appends res to the trend log, regenerates the trend
// dashboard from the full accumulated history, and logs the result — the
// CI-log evidence DW-8.1/DW-8.4 want, and the trend history/dashboard DW-8.6
// requires (regenerated every run so it is never stale relative to the log
// it is rendered from).
func recordAndReport(t *testing.T, res gate.Result) {
	t.Helper()
	res.RanAt = time.Now().UTC()
	rec := gate.RecordOf(res, gitRevForTest())
	path := trendPath(t)
	if err := gate.AppendTrend(path, rec); err != nil {
		t.Fatalf("recording trend: %v", err)
	}
	records, err := gate.LoadTrend(path)
	if err != nil {
		t.Fatalf("reloading trend history for dashboard render: %v", err)
	}
	if err := gate.WriteDashboard(repoPath(t, "docs", "eval", "dashboard.md"), records); err != nil {
		t.Fatalf("writing trend dashboard: %v", err)
	}
	t.Logf("[gate=%s verdict=%s metric=%.4f] %s", res.Gate, res.Verdict, res.Metric, res.Detail)
}

// releaseFollowingCorrelation is the deterministic per-release
// following-telemetry stand-in (see TestGate_Following's doc comment):
// followed high-utility experiences mostly succeed, followed low-utility
// ones mostly fail — a healthy, non-degenerate positive correlation. It
// computes the figure via internal/experience's own already-existing,
// already-unit-tested FollowingCorrelation (DW-5.5) — no new correlation
// math, exactly as the Design doc's Decision 7 documents.
func releaseFollowingCorrelation() float64 {
	samples := []experience.FollowingSample{
		{Utility: 0.95, Followed: true, OutcomeSuccess: true},
		{Utility: 0.9, Followed: true, OutcomeSuccess: true},
		{Utility: 0.85, Followed: true, OutcomeSuccess: true},
		{Utility: 0.8, Followed: true, OutcomeSuccess: true},
		{Utility: 0.75, Followed: true, OutcomeSuccess: true},
		{Utility: 0.7, Followed: true, OutcomeSuccess: false},
		{Utility: 0.6, Followed: true, OutcomeSuccess: true},
		{Utility: 0.5, Followed: true, OutcomeSuccess: false},
		{Utility: 0.4, Followed: true, OutcomeSuccess: false},
		{Utility: 0.3, Followed: true, OutcomeSuccess: false},
		{Utility: 0.2, Followed: true, OutcomeSuccess: false},
		{Utility: 0.1, Followed: true, OutcomeSuccess: false},
	}
	return experience.FollowingCorrelation(samples)
}
