package main

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestDW_1_3_ParseFacts_SchemaFencedAndProseWrapped covers the "returns the
// parsed facts" half of DW-1.3: the schema's {"facts": [...]} object is the
// normal shape, and a model that ignored the schema and wrapped a bare array
// in a fence or prose still yields its real facts.
func TestDW_1_3_ParseFacts_SchemaFencedAndProseWrapped(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "schema object",
			raw:  `{"facts":[{"subject":"rtd","predicate":"prefers","object":"tabs","statement":"rtd prefers tabs.","valid_at":null}]}`,
		},
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
			facts, err := parseFacts(tc.raw)
			if err != nil {
				t.Fatalf("parseFacts(%q) error = %v, want the facts", tc.raw, err)
			}
			if len(facts) != 1 || facts[0].Subject != "rtd" || facts[0].Predicate != "prefers" || facts[0].Object != "tabs" {
				t.Fatalf("parseFacts(%q) = %+v, want the single rtd/prefers/tabs fact", tc.raw, facts)
			}
		})
	}
}

// TestDW_1_3_ParseFacts_GarbageIsUnparseable pins the change from "degrade
// to []" to an error: "[]" completes the event with nothing indexed, so a
// garbled turn would lose the event's facts for good. ErrUnparseable becomes
// a retryable 502 instead.
func TestDW_1_3_ParseFacts_GarbageIsUnparseable(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty stdout", raw: ""},
		{name: "whitespace only", raw: "   \n\t  "},
		{name: "single object instead of array", raw: `{"subject":"rtd","predicate":"prefers","object":"tabs"}`},
		{name: "facts is null", raw: `{"facts":null}`},
		{name: "invalid json", raw: "[{subject: rtd, this is not json"},
		{name: "plain prose with no brackets at all", raw: "I found no durable facts in this event."},
		{name: "array of non-objects", raw: `["not", "an", "object"]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			facts, err := parseFacts(tc.raw)
			if !errors.Is(err, ErrUnparseable) {
				t.Fatalf("parseFacts(%q) = (%+v, %v), want ErrUnparseable", tc.raw, facts, err)
			}
		})
	}
}

// TestParseFacts_LegalEmptyStaysEmpty confirms a genuinely empty extraction
// ("no durable facts here") is a real answer, not an error, in both shapes.
func TestParseFacts_LegalEmptyStaysEmpty(t *testing.T) {
	for _, raw := range []string{"[]", `{"facts":[]}`} {
		facts, err := parseFacts(raw)
		if err != nil || len(facts) != 0 {
			t.Fatalf("parseFacts(%q) = (%+v, %v), want (empty, nil)", raw, facts, err)
		}
		if got := marshalFacts(facts); got != emptyFactArray {
			t.Fatalf("marshalFacts(parseFacts(%q)) = %q, want %q", raw, got, emptyFactArray)
		}
	}
}

// TestParseFacts_DropsUnexpectedExtraFields guards against a model emitting
// extra JSON fields (e.g. "confidence") that could otherwise ride along into
// engramd's decode; re-marshaling through the local fact struct strips them.
func TestParseFacts_DropsUnexpectedExtraFields(t *testing.T) {
	raw := `[{"subject":"rtd","predicate":"prefers","object":"tabs","confidence":0.9,"extra":{"nested":true}}]`
	facts, err := parseFacts(raw)
	if err != nil || len(facts) != 1 {
		t.Fatalf("parseFacts = (%+v, %v), want one fact", facts, err)
	}
	var raw2 []map[string]any
	if err := json.Unmarshal([]byte(marshalFacts(facts)), &raw2); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if _, ok := raw2[0]["confidence"]; ok {
		t.Fatalf("output %v retained unexpected field %q", raw2, "confidence")
	}
}

// TestNormalizeValidAt: engramd decodes valid_at as a time.Time, so any
// non-RFC 3339 value would reject the whole extraction. Live models return
// bare dates under the schema (seen 2026-10-25 from claude, agy and codex).
func TestNormalizeValidAt(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"2026-10-23T05:06:07Z", "2026-10-23T05:06:07Z"},
		{"2026-10-23T05:06:07.123-05:00", "2026-10-23T10:06:07Z"},
		{"2026-10-23", "2026-10-23T00:00:00Z"},
		{"last week", ""},
		{"2026-10", ""},
	}
	for _, tc := range tests {
		if got := normalizeValidAt(tc.in); got != tc.want {
			t.Errorf("normalizeValidAt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	facts, err := parseFacts(`{"facts":[{"subject":"a","predicate":"b","object":"c","statement":"s","valid_at":"2026-10-23"}]}`)
	if err != nil || facts[0].ValidAt != "2026-10-23T00:00:00Z" {
		t.Fatalf("parseFacts did not normalize valid_at: (%+v, %v)", facts, err)
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
