package store

import (
	"encoding/json"
	"testing"
)

// props unmarshals an index template's mapping properties.
func props(t *testing.T, templateJSON []byte) map[string]any {
	t.Helper()
	var tpl struct {
		Template struct {
			Settings map[string]any `json:"settings"`
			Mappings struct {
				Properties map[string]any `json:"properties"`
			} `json:"mappings"`
		} `json:"template"`
	}
	if err := json.Unmarshal(templateJSON, &tpl); err != nil {
		t.Fatalf("template JSON invalid: %v", err)
	}
	if knn, ok := tpl.Template.Settings["index.knn"].(bool); !ok || !knn {
		t.Error("settings index.knn must be true")
	}
	return tpl.Template.Mappings.Properties
}

// requireKNNVector asserts the D14/D15 vector contract on one knn_vector
// field: 1024-dim, Faiss HNSW with pinned m/ef_construction, SQfp16 encoder.
func requireKNNVector(t *testing.T, properties map[string]any, field string) {
	t.Helper()
	v, ok := properties[field].(map[string]any)
	if !ok {
		t.Fatalf("missing knn field %q", field)
	}
	if v["type"] != "knn_vector" {
		t.Errorf("%s.type = %v, want knn_vector", field, v["type"])
	}
	if dim, _ := v["dimension"].(float64); int(dim) != EmbeddingDim {
		t.Errorf("%s.dimension = %v, want %d", field, v["dimension"], EmbeddingDim)
	}
	m, ok := v["method"].(map[string]any)
	if !ok {
		t.Fatalf("%s.method missing", field)
	}
	if m["engine"] != "faiss" {
		t.Errorf("%s method.engine = %v, want faiss", field, m["engine"])
	}
	if m["name"] != "hnsw" {
		t.Errorf("%s method.name = %v, want hnsw", field, m["name"])
	}
	params, ok := m["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("%s method.parameters missing", field)
	}
	if hm, _ := params["m"].(float64); int(hm) != 16 {
		t.Errorf("%s HNSW m = %v, want 16", field, params["m"])
	}
	if efc, _ := params["ef_construction"].(float64); int(efc) != 128 {
		t.Errorf("%s HNSW ef_construction = %v, want 128", field, params["ef_construction"])
	}
	enc, ok := params["encoder"].(map[string]any)
	if !ok {
		t.Fatalf("%s SQfp16 encoder missing", field)
	}
	if enc["name"] != "sq" {
		t.Errorf("%s encoder.name = %v, want sq", field, enc["name"])
	}
	encParams, _ := enc["parameters"].(map[string]any)
	if encParams["type"] != "fp16" {
		t.Errorf("%s encoder type = %v, want fp16 (SQfp16)", field, encParams["type"])
	}
}

func requireFieldTypes(t *testing.T, properties map[string]any, want map[string]string) {
	t.Helper()
	for field, typ := range want {
		p, ok := properties[field].(map[string]any)
		if !ok {
			t.Errorf("missing mapping field %q", field)
			continue
		}
		if p["type"] != typ {
			t.Errorf("%s.type = %v, want %s", field, p["type"], typ)
		}
	}
}

// TestDW_0_3_EpisodicTemplateContract asserts the T1 template: 1024-dim
// Faiss SQfp16 knn_vector, BM25 text, tenancy/provenance fields, and the
// outbox fields (processed_at / claim_lease_until / attempts — D12/D16).
func TestDW_0_3_EpisodicTemplateContract(t *testing.T) {
	p := props(t, EpisodicTemplateJSON)
	requireKNNVector(t, p, "text_embedding")
	requireFieldTypes(t, p, map[string]string{
		"text":               "text", // BM25 target
		"event_id":           "keyword",
		"tenant_id":          "keyword",
		"team_id":            "keyword",
		"scope":              "keyword",
		"owner_agent_id":     "keyword",
		"source_ids":         "keyword",
		"occurred_at":        "date",
		"created_at":         "date",
		"processed_at":       "date",
		"claim_lease_until":  "date",
		"attempts":           "integer",
		"dead_lettered":      "boolean",
		"dead_letter_reason": "keyword",
	})
}

// TestDW_0_3_SemanticTemplateContract asserts the T2 template: the vector
// contract, BM25 statement, the four bi-temporal date fields (+ the
// invalidated_tx_at audit stamp), chain fields, and tenancy/provenance.
func TestDW_0_3_SemanticTemplateContract(t *testing.T) {
	p := props(t, SemanticTemplateJSON)
	requireKNNVector(t, p, "fact_embedding")
	requireFieldTypes(t, p, map[string]string{
		"statement":         "text", // BM25 target
		"subject":           "keyword",
		"predicate":         "keyword",
		"object":            "keyword",
		"content_key":       "keyword",
		"supersedes":        "keyword",
		"extractor_version": "keyword",
		"tenant_id":         "keyword",
		"team_id":           "keyword",
		"scope":             "keyword",
		"owner_agent_id":    "keyword",
		"source_ids":        "keyword",
		"valid_at":          "date",
		"invalid_at":        "date",
		"created_at":        "date",
		"expired_at":        "date",
		"invalidated_tx_at": "date",
	})
}

// TestDW_0_3_RRFPipelineContract asserts the fusion pipeline is rank-based
// RRF via score-ranker-processor with rank_constant 60 (D1/D14).
func TestDW_0_3_RRFPipelineContract(t *testing.T) {
	var pl struct {
		PhaseResultsProcessors []map[string]struct {
			Combination struct {
				Technique    string  `json:"technique"`
				RankConstant float64 `json:"rank_constant"`
			} `json:"combination"`
		} `json:"phase_results_processors"`
	}
	if err := json.Unmarshal(RRFPipelineJSON, &pl); err != nil {
		t.Fatalf("pipeline JSON invalid: %v", err)
	}
	if len(pl.PhaseResultsProcessors) != 1 {
		t.Fatalf("want exactly 1 phase_results_processor, got %d", len(pl.PhaseResultsProcessors))
	}
	proc, ok := pl.PhaseResultsProcessors[0]["score-ranker-processor"]
	if !ok {
		t.Fatal("missing score-ranker-processor")
	}
	if proc.Combination.Technique != "rrf" {
		t.Errorf("technique = %q, want rrf", proc.Combination.Technique)
	}
	if proc.Combination.RankConstant != 60 {
		t.Errorf("rank_constant = %v, want 60", proc.Combination.RankConstant)
	}
}

// TestLedgerTemplateContract asserts the Phase-2 extraction-ledger template
// (D13): strict mapping with the claim key fields, lifecycle phase, cached
// extraction (binary), completed actions, and the lease clock ScanIncomplete
// filters on.
func TestLedgerTemplateContract(t *testing.T) {
	var tpl struct {
		IndexPatterns []string `json:"index_patterns"`
		Template      struct {
			Mappings struct {
				Dynamic    string         `json:"dynamic"`
				Properties map[string]any `json:"properties"`
			} `json:"mappings"`
		} `json:"template"`
	}
	if err := json.Unmarshal(LedgerTemplateJSON, &tpl); err != nil {
		t.Fatalf("ledger template JSON invalid: %v", err)
	}
	if len(tpl.IndexPatterns) != 1 || tpl.IndexPatterns[0] != "engram-ledger*" {
		t.Errorf("index_patterns = %v, want [engram-ledger*]", tpl.IndexPatterns)
	}
	if tpl.Template.Mappings.Dynamic != "strict" {
		t.Errorf("dynamic = %q, want strict", tpl.Template.Mappings.Dynamic)
	}
	requireFieldTypes(t, tpl.Template.Mappings.Properties, map[string]string{
		"tenant_id":         "keyword",
		"event_id":          "keyword",
		"extractor_version": "keyword",
		"phase":             "keyword",
		"extraction":        "binary",
		"completed_actions": "keyword",
		"lease_until":       "date",
	})
}
