// Command engram-embed-server is a deterministic stand-in for the co-located
// BGE-M3 embedding service, for the local e2e stack. It serves the TEI-style
// contract (POST /embed {"inputs": [...]} -> [[float32]...]) using the same
// deterministic hashing as internal/embed.FakeEmbedder, so e2e is real-HTTP
// yet reproducible. Production swaps in a real BGE-M3 container behind the
// identical contract (D15) — engramd points -embed-url at whichever is running.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/ryanthedev/engram/internal/embed"
	"github.com/ryanthedev/engram/internal/store"
)

func main() {
	addr := flag.String("addr", ":8081", "listen address")
	dim := flag.Int("dim", store.EmbeddingDim, "embedding dimension (must match the index template)")
	flag.Parse()

	embedder := embed.NewFakeEmbedder(*dim, nil)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/embed", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Inputs []string `json:"inputs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		vecs, err := embedder.Embed(r.Context(), req.Inputs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(vecs)
	})

	slog.Info("engram-embed-server listening", "addr", *addr, "dim", *dim)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "engram-embed-server:", err)
		os.Exit(1)
	}
}
