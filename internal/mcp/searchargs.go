package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// searchParams is the memory_search argument vocabulary — the complete, closed
// set of keys the tool accepts. It is the allowlist the barricade checks
// against, AND the vocabulary every rejection names back to the caller, so the
// two can never drift apart: an LLM that guesses "kinds" or "before" is told
// what it may say instead and can self-correct on the next call.
var searchParams = []string{
	"query", "k",
	"kind", "subject", "predicate", "object", "extractor_version",
	"since", "until", "include_superseded", "sources",
}

// memorySources is the source vocabulary memory_search accepts: the two built-in
// tiers plus the sources engram-server registers (experience tier, graph
// expander). The retrieval registry remains the AUTHORITY — it validates the
// names again and would reject one this list wrongly admits — but the tool
// checks them here so a typo costs the caller an immediate, self-correcting
// error instead of a network round-trip. Same posture, same reason, as
// readSources. Keep in sync with cmd/engram-server's RegisterTier/RegisterPostHook
// calls.
var memorySources = []string{"episodic", "semantic", "experience", "graph"}

// searchArgs is the decoded memory_search argument object. Since/Until arrive as
// strings (JSON has no time type) and are parsed at the barricade, so nothing
// downstream ever re-parses caller text.
type searchArgs struct {
	Query             string   `json:"query"`
	K                 int      `json:"k"`
	Kind              string   `json:"kind"`
	Subject           string   `json:"subject"`
	Predicate         string   `json:"predicate"`
	Object            string   `json:"object"`
	ExtractorVersion  string   `json:"extractor_version"`
	Since             string   `json:"since"`
	Until             string   `json:"until"`
	IncludeSuperseded bool     `json:"include_superseded"`
	Sources           []string `json:"sources"`
}

// parseSearchArgs is the memory_search barricade. Arguments arrive from an
// agent — external input — so they are fully validated HERE, before the backend
// is called: an invalid request costs zero network round-trips and never reaches
// the retriever (DW-5.4). Everything downstream may assume a well-formed query,
// a bounded k, parsed time bounds, and a known source vocabulary.
//
// The returned error is caller-facing text: it names the valid vocabulary rather
// than merely reporting that something was wrong.
func parseSearchArgs(raw json.RawMessage) (query string, k int, f SearchFilter, err error) {
	// Decode twice on purpose. The first pass is the allowlist check: it names
	// the offending key exactly, which encoding/json's own unknown-field error
	// does not do in a form worth showing an LLM. Only then is the value shape
	// decoded.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return "", 0, SearchFilter{}, fmt.Errorf("memory_search arguments must be a JSON object; valid parameters: %s", strings.Join(searchParams, ", "))
	}
	for key := range keys {
		if !slices.Contains(searchParams, key) {
			return "", 0, SearchFilter{}, fmt.Errorf("memory_search: unknown parameter %q; valid parameters: %s", key, strings.Join(searchParams, ", "))
		}
	}
	var args searchArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		// A per-field type mismatch (e.g. kind: 7) comes back from encoding/json
		// as an *UnmarshalTypeError naming the offending JSON field. Report that
		// field and its expected shape in caller-facing terms — never the raw
		// error, which names the Go struct and type (e.g. "searchArgs.kind of
		// type string") and would leak implementation detail into the vocabulary
		// every other rejection in this file names cleanly.
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return "", 0, SearchFilter{}, fmt.Errorf("memory_search: %s must be %s, got %s",
				typeErr.Field, expectedShape(typeErr.Type), typeErr.Value)
		}
		return "", 0, SearchFilter{}, fmt.Errorf("memory_search: invalid argument value: %v", err)
	}
	if args.Query == "" {
		return "", 0, SearchFilter{}, fmt.Errorf("memory_search requires a non-empty query")
	}

	f = SearchFilter{
		Kind:              args.Kind,
		Subject:           args.Subject,
		Predicate:         args.Predicate,
		Object:            args.Object,
		ExtractorVersion:  args.ExtractorVersion,
		IncludeSuperseded: args.IncludeSuperseded,
		Sources:           args.Sources,
	}
	if f.Since, err = parseBound("since", args.Since); err != nil {
		return "", 0, SearchFilter{}, err
	}
	if f.Until, err = parseBound("until", args.Until); err != nil {
		return "", 0, SearchFilter{}, err
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && f.Since.After(f.Until) {
		return "", 0, SearchFilter{}, fmt.Errorf("memory_search: since (%s) is after until (%s); the time range is empty",
			f.Since.Format(time.RFC3339), f.Until.Format(time.RFC3339))
	}
	if err := validateSources(args.Sources); err != nil {
		return "", 0, SearchFilter{}, err
	}

	// k <= 0 is "server-chosen", not an error: request generously, pack tightly.
	// The upper bound is enforced by the retriever's clamp to [1, MaxK], which
	// is the one place that knows it.
	k = args.K
	if k <= 0 {
		k = defaultRequestK
	}
	return args.Query, k, f, nil
}

// expectedShape describes a searchArgs field's declared JSON shape in
// caller-facing terms, derived from its Go kind rather than its Go type name —
// the caller sees "a string" or "an array of strings", never "string" as in a
// Go struct field.
func expectedShape(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice, reflect.Array:
		return "an array of strings"
	default:
		return "a different type"
	}
}

// parseBound parses one RFC 3339 time bound; an absent bound is the zero time
// (open interval), never an error.
func parseBound(name, raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("memory_search: %s must be an RFC 3339 timestamp (e.g. 2026-07-12T00:00:00Z), got %q", name, raw)
	}
	return t.UTC(), nil
}

// validateSources checks the source vocabulary. A nil list means every source; an
// EMPTY list is an error, deliberately — "search nothing" and "search
// everything" must never be the same request (retrieval draws the same line, for
// the same reason).
func validateSources(sources []string) error {
	if sources == nil {
		return nil
	}
	if len(sources) == 0 {
		return fmt.Errorf("memory_search: sources is empty; omit it to search every source, or name at least one of: %s", strings.Join(memorySources, ", "))
	}
	for _, s := range sources {
		if !slices.Contains(memorySources, s) {
			return fmt.Errorf("memory_search: unknown source %q; valid sources: %s", s, strings.Join(memorySources, ", "))
		}
	}
	return nil
}
