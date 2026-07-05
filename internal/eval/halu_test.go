package eval_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ryanthedev/engram/internal/eval"
	"github.com/ryanthedev/engram/internal/retrieval"
)

// haluFixedRetriever returns canned statements per query text, mirroring
// harness_test.go's fixedRetriever pattern — no cluster needed.
type haluFixedRetriever map[string][]string

func (f haluFixedRetriever) Search(_ context.Context, q retrieval.Query, _ retrieval.Filter) ([]retrieval.Hit, error) {
	stmts, ok := f[q.Text]
	if !ok {
		return nil, errors.New("no fixture for query")
	}
	hits := make([]retrieval.Hit, len(stmts))
	for i, s := range stmts {
		hits[i] = retrieval.Hit{ID: "h", Score: 1, Fields: map[string]any{"statement": s}}
	}
	return hits, nil
}

func TestRuleHaluJudge_GroundedStatementSupported(t *testing.T) {
	corpus := "orbitctl sets a hard rpc timeout of 4000ms for the scheduler service."
	ok, err := eval.RuleHaluJudge{}.Supported(context.Background(), corpus, "orbitctl rpc timeout scheduler")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("statement built entirely from corpus vocabulary must be supported")
	}
}

func TestRuleHaluJudge_UnrelatedStatementUnsupported(t *testing.T) {
	corpus := "orbitctl sets a hard rpc timeout of 4000ms for the scheduler service."
	ok, err := eval.RuleHaluJudge{}.Supported(context.Background(), corpus, "shadow-directorate controls the nuclear launch codes")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("statement sharing zero vocabulary with the corpus must be unsupported")
	}
}

// TestRuleHaluJudge_AtThresholdBoundary pins the "exactly at threshold is
// supported" contract (a ">=" boundary, not ">"), matching the regression
// gate's non-inferiority shape. Four 3+-char tokens, exactly 2 in-corpus: 0.5
// overlap == RuleTokenOverlapThreshold.
func TestRuleHaluJudge_AtThresholdBoundary(t *testing.T) {
	corpus := "aaa bbb"
	atThreshold := "aaa bbb ccc ddd" // 2/4 = 0.5, exactly the threshold
	ok, err := eval.RuleHaluJudge{}.Supported(context.Background(), corpus, atThreshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("overlap exactly at RuleTokenOverlapThreshold (%v) must be supported (>= contract)", eval.RuleTokenOverlapThreshold)
	}
	belowThreshold := "aaa bbb ccc ddd eee" // 2/5 = 0.4, just under
	ok, err = eval.RuleHaluJudge{}.Supported(context.Background(), corpus, belowThreshold)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("overlap just under the threshold must be unsupported")
	}
}

// TestRuleHaluJudge_EmptyStatementUnsupported: a degenerate/empty statement
// asserts nothing checkable — must not be vacuously "supported".
func TestRuleHaluJudge_EmptyStatementUnsupported(t *testing.T) {
	ok, err := eval.RuleHaluJudge{}.Supported(context.Background(), "anything here", "   ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("an empty statement must not be treated as supported")
	}
}

func TestRunHallucinationSuite_AllGroundedReportsZeroRate(t *testing.T) {
	gs := eval.HaluGoldSet{
		Corpus: []eval.Doc{{ID: "d1", Text: "orbitctl sets a hard rpc timeout of 4000ms for the scheduler service."}},
		Cases:  []eval.HaluCase{{ID: "c1", Query: "what is orbitctl's timeout"}},
	}
	r := haluFixedRetriever{"what is orbitctl's timeout": {"orbitctl rpc timeout scheduler 4000ms"}}
	rep, err := eval.RunHallucinationSuite(context.Background(), r, gs, eval.RuleHaluJudge{}, 10, retrieval.Filter{ValidOnly: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.Cases != 1 || rep.Assertions != 1 || rep.Hallucinated != 0 || rep.Rate != 0 {
		t.Fatalf("got %+v, want 1 case, 1 assertion, 0 hallucinated, rate 0", rep)
	}
}

func TestRunHallucinationSuite_UngroundedAssertionCountsAsHallucination(t *testing.T) {
	gs := eval.HaluGoldSet{
		Corpus: []eval.Doc{{ID: "d1", Text: "orbitctl sets a hard rpc timeout of 4000ms for the scheduler service."}},
		Cases:  []eval.HaluCase{{ID: "c1", Query: "q"}},
	}
	r := haluFixedRetriever{"q": {"shadow-directorate controls the nuclear launch codes"}}
	rep, err := eval.RunHallucinationSuite(context.Background(), r, gs, eval.RuleHaluJudge{}, 10, retrieval.Filter{ValidOnly: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.Hallucinated != 1 || rep.Rate != 1 {
		t.Fatalf("got %+v, want 1 hallucinated, rate 1.0", rep)
	}
	if len(rep.Details) != 1 || len(rep.Details[0].Unsupported) != 1 {
		t.Fatalf("unsupported statement not recorded in details: %+v", rep.Details)
	}
}

// TestRunHallucinationSuite_MixedRate pins the rate math on a hand-computable
// case: 1 of 2 asserted statements is grounded.
func TestRunHallucinationSuite_MixedRate(t *testing.T) {
	gs := eval.HaluGoldSet{
		Corpus: []eval.Doc{{ID: "d1", Text: "orbitctl sets a hard rpc timeout of 4000ms for the scheduler service."}},
		Cases:  []eval.HaluCase{{ID: "c1", Query: "q"}},
	}
	r := haluFixedRetriever{"q": {
		"orbitctl rpc timeout scheduler 4000ms",       // grounded
		"shadow-directorate seized the launch codes", // ungrounded
	}}
	rep, err := eval.RunHallucinationSuite(context.Background(), r, gs, eval.RuleHaluJudge{}, 10, retrieval.Filter{ValidOnly: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.Assertions != 2 || rep.Hallucinated != 1 || rep.Rate != 0.5 {
		t.Fatalf("got %+v, want 2 assertions, 1 hallucinated, rate 0.5", rep)
	}
}

// TestRunHallucinationSuite_NoAssertionsIsZeroNotNaN: a query that returns no
// statement-bearing hits (e.g. only episodic "text" hits, or genuinely no
// hits) must report rate 0, not divide-by-zero / NaN.
func TestRunHallucinationSuite_NoAssertionsIsZeroNotNaN(t *testing.T) {
	gs := eval.HaluGoldSet{
		Corpus: []eval.Doc{{ID: "d1", Text: "x"}},
		Cases:  []eval.HaluCase{{ID: "c1", Query: "q"}},
	}
	r := haluFixedRetriever{"q": {}}
	rep, err := eval.RunHallucinationSuite(context.Background(), r, gs, eval.RuleHaluJudge{}, 10, retrieval.Filter{ValidOnly: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.Rate != 0 {
		t.Fatalf("rate = %v, want 0", rep.Rate)
	}
}

// TestRunHallucinationSuite_RetrievalErrorNotFatal: a per-case retrieval
// error is recorded, not fatal to the whole run (mirrors harness.go's Run
// error handling).
func TestRunHallucinationSuite_RetrievalErrorNotFatal(t *testing.T) {
	gs := eval.HaluGoldSet{
		Corpus: []eval.Doc{{ID: "d1", Text: "x"}},
		Cases:  []eval.HaluCase{{ID: "c1", Query: "no-fixture-for-this"}},
	}
	r := haluFixedRetriever{}
	rep, err := eval.RunHallucinationSuite(context.Background(), r, gs, eval.RuleHaluJudge{}, 10, retrieval.Filter{ValidOnly: true})
	if err != nil {
		t.Fatalf("a per-case retrieval error must not fail the whole run: %v", err)
	}
	if rep.Cases != 1 || rep.Assertions != 0 {
		t.Fatalf("got %+v, want 1 case scored with 0 assertions", rep)
	}
}

// TestRunHallucinationSuite_JudgeErrorFailsClosed: a judge that errors must
// count the statement as a hallucination, never silently pass it.
func TestRunHallucinationSuite_JudgeErrorFailsClosed(t *testing.T) {
	gs := eval.HaluGoldSet{
		Corpus: []eval.Doc{{ID: "d1", Text: "x"}},
		Cases:  []eval.HaluCase{{ID: "c1", Query: "q"}},
	}
	r := haluFixedRetriever{"q": {"anything"}}
	rep, err := eval.RunHallucinationSuite(context.Background(), r, gs, erroringJudge{}, 10, retrieval.Filter{ValidOnly: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.Hallucinated != 1 || rep.Details[0].JudgeErrored != 1 {
		t.Fatalf("got %+v, want the errored judgment counted as hallucinated", rep)
	}
}

type erroringJudge struct{}

func (erroringJudge) Supported(context.Context, string, string) (bool, error) {
	return true, errors.New("judge unavailable") // even if it claims true, the error must win
}

func TestRunHallucinationSuite_RejectsBadK(t *testing.T) {
	if _, err := eval.RunHallucinationSuite(context.Background(), haluFixedRetriever{}, eval.HaluGoldSet{}, eval.RuleHaluJudge{}, 0, retrieval.Filter{ValidOnly: true}); err == nil {
		t.Fatal("k=0 must error")
	}
}

func TestRunHallucinationSuite_RejectsNilJudge(t *testing.T) {
	if _, err := eval.RunHallucinationSuite(context.Background(), haluFixedRetriever{}, eval.HaluGoldSet{}, nil, 10, retrieval.Filter{ValidOnly: true}); err == nil {
		t.Fatal("nil judge must error")
	}
}

// TestHaluFixtures_SelfConsistent proves the checked-in fixture corpus is
// internally well-formed and that a faithful "extractor" (one that only ever
// echoes corpus facts back) scores hallucination rate 0 on it — the fixture
// must not be tautologically broken.
func TestHaluFixtures_SelfConsistent(t *testing.T) {
	gs := eval.HaluFixtures()
	if len(gs.Corpus) == 0 || len(gs.Cases) == 0 {
		t.Fatal("HaluFixtures must return a non-empty corpus and case set")
	}
	if len(gs.Corpus) != len(gs.Cases) {
		t.Fatalf("corpus/cases size mismatch: %d docs, %d cases", len(gs.Corpus), len(gs.Cases))
	}
	seen := map[string]bool{}
	for _, d := range gs.Corpus {
		if seen[d.ID] {
			t.Fatalf("duplicate corpus doc id %q", d.ID)
		}
		seen[d.ID] = true
	}
	// A retriever that echoes each case's matching corpus doc verbatim
	// (a "perfect, faithful extractor") must score rate 0.
	fixed := haluFixedRetriever{}
	for i, c := range gs.Cases {
		fixed[c.Query] = []string{gs.Corpus[i].Text}
	}
	rep, err := eval.RunHallucinationSuite(context.Background(), fixed, gs, eval.RuleHaluJudge{}, 10, retrieval.Filter{ValidOnly: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rep.Rate != 0 {
		t.Fatalf("a faithful extractor over the fixture corpus scored rate %v, want 0: %+v", rep.Rate, rep.Details)
	}
}

// --- HTTPHaluJudge ---

func TestHTTPHaluJudge_SupportedTrue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHaluChoice(w, `{"supported": true, "reason": "matches"}`)
	}))
	defer srv.Close()
	j := eval.NewHTTPHaluJudge(srv.Client(), srv.URL, "test-model")
	ok, err := j.Supported(context.Background(), "corpus", "statement")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("want supported=true")
	}
}

func TestHTTPHaluJudge_SupportedFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHaluChoice(w, `{"supported": false, "reason": "no match"}`)
	}))
	defer srv.Close()
	j := eval.NewHTTPHaluJudge(srv.Client(), srv.URL, "test-model")
	ok, err := j.Supported(context.Background(), "corpus", "statement")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("want supported=false")
	}
}

// TestHTTPHaluJudge_NonOKStatusFailsClosed is the dirty test: a non-2xx
// response must return an error and false, never true.
func TestHTTPHaluJudge_NonOKStatusFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	j := eval.NewHTTPHaluJudge(srv.Client(), srv.URL, "test-model")
	ok, err := j.Supported(context.Background(), "corpus", "statement")
	if err == nil {
		t.Fatal("non-2xx response must error")
	}
	if ok {
		t.Fatal("a failed judge call must never report supported=true")
	}
}

// TestHTTPHaluJudge_UnparseableVerdictFailsClosed is a dirty test: garbage
// content must error and report false.
func TestHTTPHaluJudge_UnparseableVerdictFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHaluChoice(w, "not json at all")
	}))
	defer srv.Close()
	j := eval.NewHTTPHaluJudge(srv.Client(), srv.URL, "test-model")
	ok, err := j.Supported(context.Background(), "corpus", "statement")
	if err == nil {
		t.Fatal("unparseable verdict must error")
	}
	if ok {
		t.Fatal("unparseable verdict must never report supported=true")
	}
}

// TestHTTPHaluJudge_NoChoicesFailsClosed is a dirty test: an empty choices
// array must error and report false.
func TestHTTPHaluJudge_NoChoicesFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
	}))
	defer srv.Close()
	j := eval.NewHTTPHaluJudge(srv.Client(), srv.URL, "test-model")
	if ok, err := j.Supported(context.Background(), "corpus", "statement"); err == nil || ok {
		t.Fatalf("empty choices must fail closed, got ok=%v err=%v", ok, err)
	}
}

// TestHTTPHaluJudge_TransportErrorFailsClosed is a dirty test: an
// unreachable endpoint must error and report false.
func TestHTTPHaluJudge_TransportErrorFailsClosed(t *testing.T) {
	j := eval.NewHTTPHaluJudge(http.DefaultClient, "http://127.0.0.1:1", "test-model")
	if ok, err := j.Supported(context.Background(), "corpus", "statement"); err == nil || ok {
		t.Fatalf("unreachable endpoint must fail closed, got ok=%v err=%v", ok, err)
	}
}

func writeHaluChoice(w http.ResponseWriter, content string) {
	resp := map[string]any{
		"choices": []map[string]any{
			{"message": map[string]any{"role": "assistant", "content": content}},
		},
	}
	_ = json.NewEncoder(w).Encode(resp)
}
