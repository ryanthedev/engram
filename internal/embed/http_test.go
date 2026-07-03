package embed_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/embed"
)

// fakeTEI simulates a TEI-style embedding endpoint: POST /embed with
// {"inputs": [...]} returns a JSON array of vectors, index-aligned.
func fakeTEI(t *testing.T, dim int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embed" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			Inputs []string `json:"inputs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		vectors := make([][]float32, len(req.Inputs))
		for i := range vectors {
			v := make([]float32, dim)
			for j := range v {
				v[j] = float32(i + 1)
			}
			vectors[i] = v
		}
		_ = json.NewEncoder(w).Encode(vectors)
	}))
}

// TestHTTPEmbedderEmbedsAndAligns proves the request/response shape against
// a TEI-style fake and index alignment of the returned vectors.
func TestHTTPEmbedderEmbedsAndAligns(t *testing.T) {
	srv := fakeTEI(t, 8)
	defer srv.Close()

	e := embed.NewHTTPEmbedder(srv.Client(), srv.URL, embed.ModelInfo{Model: "BAAI/bge-m3", Revision: "abc123", Dim: 8})
	vecs, err := e.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("got %d vectors, want 3", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != 8 {
			t.Fatalf("vector %d has dim %d, want 8", i, len(v))
		}
		if v[0] != float32(i+1) {
			t.Errorf("vector %d not index-aligned: v[0]=%v, want %v", i, v[0], i+1)
		}
	}
}

// TestHTTPEmbedderEmptyInput short-circuits without a request.
func TestHTTPEmbedderEmptyInput(t *testing.T) {
	e := embed.NewHTTPEmbedder(http.DefaultClient, "http://unused.invalid", embed.ModelInfo{Model: "m", Revision: "r", Dim: 4})
	vecs, err := e.Embed(context.Background(), nil)
	if err != nil || vecs != nil {
		t.Fatalf("empty input: got (%v, %v), want (nil, nil)", vecs, err)
	}
}

// TestHTTPEmbedderRejectsServerError surfaces a non-2xx response as an error
// carrying the body.
func TestHTTPEmbedderRejectsServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("model loading"))
	}))
	defer srv.Close()

	e := embed.NewHTTPEmbedder(srv.Client(), srv.URL, embed.ModelInfo{Model: "m", Revision: "r", Dim: 4})
	_, err := e.Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "model loading") {
		t.Fatalf("want error containing service body, got: %v", err)
	}
}

// TestHTTPEmbedderRejectsVectorCountMismatch guards against a
// misconfigured/misbehaving embedding service silently truncating output.
func TestHTTPEmbedderRejectsVectorCountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([][]float32{{1, 2, 3}})
	}))
	defer srv.Close()

	e := embed.NewHTTPEmbedder(srv.Client(), srv.URL, embed.ModelInfo{Model: "m", Revision: "r", Dim: 3})
	_, err := e.Embed(context.Background(), []string{"x", "y"})
	if err == nil || !strings.Contains(err.Error(), "1 vectors for 2 inputs") {
		t.Fatalf("want vector-count-mismatch error, got: %v", err)
	}
}

// TestHTTPEmbedderInfo proves Info reports the configured pin.
func TestHTTPEmbedderInfo(t *testing.T) {
	e := embed.NewHTTPEmbedder(http.DefaultClient, "http://unused.invalid", embed.ModelInfo{Model: "BAAI/bge-m3", Revision: "rev1", Dim: 1024})
	if got := e.Info(); got.Model != "BAAI/bge-m3" || got.Revision != "rev1" || got.Dim != 1024 {
		t.Errorf("Info() = %+v, want the configured pin", got)
	}
}
