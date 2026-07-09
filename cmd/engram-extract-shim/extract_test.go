package main

import (
	"encoding/json"
	"testing"
)

// TestDW_1_3_ParseFacts_FencedAndProseWrapped covers the "returns the parsed
// array" half of DW-1.3: when the model wraps a legal fact array in a
// markdown fence or surrounding prose, the shim recovers the real facts
// rather than throwing them away.
func TestDW_1_3_ParseFacts_FencedAndProseWrapped(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "fenced with json language tag",
			raw:  "```json\n[{\"subject\":\"rtd\",\"predicate\":\"prefers\",\"object\":\"tabs\"}]\n```",
		},
		{
			name: "bare fence no language tag",
			raw:  "```\n[{\"subject\":\"rtd\",\"predicate\":\"prefers\",\"object\":\"tabs\"}]\n```",
		},
		{
			name: "prose wrapped, no fence",
			raw:  "Here are the facts:\n[{\"subject\":\"rtd\",\"predicate\":\"prefers\",\"object\":\"tabs\"}]\nHope that helps!",
		},
		{
			name: "prose and fence together",
			raw:  "Sure thing.\n```json\n[{\"subject\":\"rtd\",\"predicate\":\"prefers\",\"object\":\"tabs\"}]\n```\nLet me know if you need more.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, count := parseFacts(tc.raw)
			if count != 1 {
				t.Fatalf("parseFacts(%q) count = %d, want 1 (facts should survive, not degrade)", tc.raw, count)
			}
			var facts []fact
			if err := json.Unmarshal(out, &facts); err != nil {
				t.Fatalf("parseFacts output not valid JSON: %v (%s)", err, out)
			}
			if len(facts) != 1 || facts[0].Subject != "rtd" || facts[0].Predicate != "prefers" || facts[0].Object != "tabs" {
				t.Fatalf("parseFacts(%q) = %+v, want the single rtd/prefers/tabs fact", tc.raw, facts)
			}
		})
	}
}

// TestDW_1_3_ParseFacts_GarbageDegradesToEmptyArray covers the "degrades to
// []" half of DW-1.3: structurally broken model output never propagates as
// an error and never becomes anything but a legal empty array.
func TestDW_1_3_ParseFacts_GarbageDegradesToEmptyArray(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty stdout", raw: ""},
		{name: "whitespace only", raw: "   \n\t  "},
		{name: "object instead of array", raw: `{"subject":"rtd","predicate":"prefers","object":"tabs"}`},
		{name: "invalid json", raw: "[{subject: rtd, this is not json"},
		{name: "plain prose with no brackets at all", raw: "I found no durable facts in this event."},
		{name: "array of non-objects", raw: `["not", "an", "object"]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, count := parseFacts(tc.raw)
			if string(out) != emptyFactArray {
				t.Fatalf("parseFacts(%q) = %q, want %q", tc.raw, out, emptyFactArray)
			}
			if count != 0 {
				t.Fatalf("parseFacts(%q) count = %d, want 0", tc.raw, count)
			}
		})
	}
}

// TestParseFacts_LegalEmptyArrayStaysEmpty confirms a genuinely empty
// extraction ("no durable facts here") round-trips as [] rather than being
// mistaken for garbage — the two cases must be indistinguishable on the
// wire (both are the legal ErrNoFacts case downstream) but this pins that
// the happy path for zero facts still produces valid output.
func TestParseFacts_LegalEmptyArrayStaysEmpty(t *testing.T) {
	out, count := parseFacts("[]")
	if string(out) != emptyFactArray || count != 0 {
		t.Fatalf("parseFacts(\"[]\") = (%q, %d), want (%q, 0)", out, count, emptyFactArray)
	}
}

// TestParseFacts_DropsUnexpectedExtraFields guards against a model emitting
// extra JSON fields (e.g. "confidence") that could otherwise ride along into
// engramd's decode; re-marshaling through the local fact struct strips them.
func TestParseFacts_DropsUnexpectedExtraFields(t *testing.T) {
	raw := `[{"subject":"rtd","predicate":"prefers","object":"tabs","confidence":0.9,"extra":{"nested":true}}]`
	out, count := parseFacts(raw)
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	var raw2 []map[string]any
	if err := json.Unmarshal(out, &raw2); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if _, ok := raw2[0]["confidence"]; ok {
		t.Fatalf("output %s retained unexpected field %q", out, "confidence")
	}
}

func TestStripCodeFences(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{name: "no fence passthrough", in: "[1,2,3]", want: "[1,2,3]"},
		// stripCodeFences mirrors internal/ingest/extraction.go's implementation
		// byte-for-byte, including that it leaves a trailing newline before the
		// closing fence — extractArraySubstring (called next in the parseFacts
		// pipeline) trims that away when it locates the closing ']'.
		{name: "json tagged fence", in: "```json\n[1,2,3]\n```", want: "[1,2,3]\n"},
		{name: "bare fence", in: "```\n[1,2,3]\n```", want: "[1,2,3]\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripCodeFences(tc.in); got != tc.want {
				t.Fatalf("stripCodeFences(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExtractArraySubstring(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{name: "clean array", in: "[1,2]", want: "[1,2]"},
		{name: "leading and trailing prose", in: "here: [1,2] thanks", want: "[1,2]"},
		{name: "empty input", in: "", want: ""},
		{name: "object with no array brackets", in: `{"a":1}`, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractArraySubstring(tc.in); got != tc.want {
				t.Fatalf("extractArraySubstring(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
