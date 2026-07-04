package graph

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newJudgeServer(t *testing.T, handler http.HandlerFunc) *HTTPJudge {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewHTTPJudge(srv.Client(), srv.URL, "test-model")
}

func TestHTTPJudge_ParsesSameEntityTrue(t *testing.T) {
	j := newJudgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJudgeChoice(w, `{"same_entity": true, "reason": "clearly the same"}`)
	})
	same, reason, err := j.SameEntity(context.Background(), Candidate{Name: "a"}, Candidate{Name: "a"})
	if err != nil {
		t.Fatalf("SameEntity: %v", err)
	}
	if !same {
		t.Errorf("same = false, want true")
	}
	if reason != "clearly the same" {
		t.Errorf("reason = %q", reason)
	}
}

func TestHTTPJudge_ParsesSameEntityFalse(t *testing.T) {
	j := newJudgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJudgeChoice(w, `{"same_entity": false, "reason": "different senses"}`)
	})
	same, _, err := j.SameEntity(context.Background(), Candidate{Name: "a"}, Candidate{Name: "a"})
	if err != nil {
		t.Fatalf("SameEntity: %v", err)
	}
	if same {
		t.Errorf("same = true, want false")
	}
}

// TestHTTPJudge_UnparseableResponseErrors: the fail-safe default (Decide
// treating an error as "distinct") is the CALLER's job — HTTPJudge itself
// must surface the error rather than guess.
func TestHTTPJudge_UnparseableResponseErrors(t *testing.T) {
	j := newJudgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJudgeChoice(w, "not json at all")
	})
	if _, _, err := j.SameEntity(context.Background(), Candidate{Name: "a"}, Candidate{Name: "a"}); err == nil {
		t.Fatal("unparseable judge output should error")
	}
}

func TestHTTPJudge_NonSuccessStatusErrors(t *testing.T) {
	j := newJudgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})
	if _, _, err := j.SameEntity(context.Background(), Candidate{Name: "a"}, Candidate{Name: "a"}); err == nil {
		t.Fatal("non-2xx response should error")
	}
}

func TestHTTPJudge_EmptyChoicesErrors(t *testing.T) {
	j := newJudgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := judgeChatResponse{}
		b, _ := json.Marshal(resp)
		_, _ = w.Write(b)
	})
	if _, _, err := j.SameEntity(context.Background(), Candidate{Name: "a"}, Candidate{Name: "a"}); err == nil {
		t.Fatal("empty choices should error")
	}
}

// TestHTTPJudge_StripsCodeFences: models often wrap JSON in ```json fences
// despite instructions.
func TestHTTPJudge_StripsCodeFences(t *testing.T) {
	j := newJudgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJudgeChoice(w, "```json\n{\"same_entity\": true, \"reason\": \"fenced\"}\n```")
	})
	same, reason, err := j.SameEntity(context.Background(), Candidate{Name: "a"}, Candidate{Name: "a"})
	if err != nil {
		t.Fatalf("SameEntity: %v", err)
	}
	if !same || reason != "fenced" {
		t.Errorf("same=%v reason=%q, want true/fenced", same, reason)
	}
}

// TestHTTPJudge_UsedThroughDeduper_ErrorDefaultsToDistinct proves the
// end-to-end fail-safe wiring: an HTTPJudge that errors (server unreachable)
// resolves an ambiguous-band Decide call to "distinct", never Merge.
func TestHTTPJudge_UsedThroughDeduper_ErrorDefaultsToDistinct(t *testing.T) {
	j := newJudgeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	d := mustDeduper(t, j)
	a := vec(1, 0, 0, 0)
	b := vec(0.6, 0.8, 0, 0)
	dec, err := d.Decide(context.Background(), Candidate{Name: "widget", Embedding: b},
		[]Candidate{{ID: "e1", Name: "widget", Embedding: a}})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.Merge {
		t.Fatal("HTTPJudge failure through Decide must default to distinct")
	}
}

func writeJudgeChoice(w http.ResponseWriter, content string) {
	resp := judgeChatResponse{Choices: []struct {
		Message judgeChatMessage `json:"message"`
	}{{Message: judgeChatMessage{Role: "assistant", Content: content}}}}
	b, _ := json.Marshal(resp)
	_, _ = w.Write(b)
}
