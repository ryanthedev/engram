package ingest_test

import (
	"testing"

	"github.com/ryanthedev/engram/internal/ingest"
)

// TestIsFactDirectiveLine pins the directive-line detector the CLI ingest
// advisory (internal/cli) relies on: it must fire on fact:/retract: prefixes
// (with surrounding whitespace tolerated) and stay false on plain prose.
func TestIsFactDirectiveLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"fact prefix", "fact: alice | likes | tea", true},
		{"retract prefix", "retract: alice | likes", true},
		{"leading/trailing whitespace tolerated", "   fact: a | b | c   ", true},
		{"plain prose", "alice really likes her tea in the morning", false},
		{"prose mentioning fact mid-line", "the fact is alice likes tea", false},
		{"empty line", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ingest.IsFactDirectiveLine(tt.line); got != tt.want {
				t.Errorf("IsFactDirectiveLine(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// TestParseFactDirectiveLine pins well-formed vs malformed for both
// directive kinds — the DW-4.3 "space-delimited fact: line is malformed"
// case in particular — using the SAME parser the extractor runs, so the CLI
// advisory can never disagree with what actually gets extracted.
func TestParseFactDirectiveLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"well-formed fact", "fact: alice | prefers | dark-mode", true},
		{"well-formed fact with valid_at", "fact: alice | prefers | dark-mode @ 2026-06-01T12:00:00Z", true},
		{"well-formed retract", "retract: alice | prefers", true},
		{"space-delimited fact (malformed, DW-4.3)", "fact: alice prefers dark-mode", false},
		{"retract missing predicate", "retract: alice", false},
		{"fact missing object", "fact: alice | prefers", false},
		{"non-directive line", "alice prefers dark mode, obviously", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ingest.ParseFactDirectiveLine(tt.line); got != tt.want {
				t.Errorf("ParseFactDirectiveLine(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}
