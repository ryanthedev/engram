package cli

import (
	"fmt"
	"strings"

	"github.com/ryanthedev/engram/internal/experience"
	"github.com/ryanthedev/engram/internal/ingest"
)

// directiveGrammar names the expected pipe-delimited grammar for each
// directive kind, keyed by its prefix. Used only to render the advisory
// message — the actual parsing is delegated to internal/ingest and
// internal/experience so the message and the parser can never drift apart.
var directiveGrammar = map[string]string{
	"fact:":       `fact: subject | predicate | object [@ RFC3339 valid_at]`,
	"retract:":    `retract: subject | predicate [@ RFC3339 valid_at]`,
	"experience:": `experience: task | distilled skill | success|failure | phi=0..1 | evidence=... | signals=... | context=...`,
}

// directiveAdvisory scans ingest text for a directive-looking line — one
// starting with fact:/retract:/experience: — that fails to parse under the
// SAME authoritative grammar the extraction/distillation pipeline uses
// (internal/ingest.ParseFactDirectiveLine, internal/experience.ParseExperienceLine).
// It returns a human-readable warning naming every malformed line, or ""
// when none are malformed.
//
// Plain prose — any line matching no directive prefix — is silently
// ignored: prose is the correct, expected input for the production LLM
// extractor (HTTPExtractor), so warning on "no directive found" would cry
// wolf on every legitimate ingest. This function only ever flags a line that
// LOOKS like a directive and isn't one.
func directiveAdvisory(text string) string {
	var bad []string
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		switch {
		case ingest.IsFactDirectiveLine(trimmed) && !ingest.ParseFactDirectiveLine(trimmed):
			bad = append(bad, malformedLine(i+1, trimmed, factPrefix(trimmed)))
		case experience.IsExperienceDirective(trimmed) && !experience.ParseExperienceLine(trimmed):
			bad = append(bad, malformedLine(i+1, trimmed, "experience:"))
		}
	}
	if len(bad) == 0 {
		return ""
	}
	return "engram: warning: " + strings.Join(bad, "; ")
}

// factPrefix reports which of the two fact-package prefixes a directive-look
// line carries, for the advisory message only (parsing itself is left to
// ingest.ParseFactDirectiveLine).
func factPrefix(line string) string {
	if strings.HasPrefix(line, "retract:") {
		return "retract:"
	}
	return "fact:"
}

// malformedLine renders one advisory entry: the 1-based line number, the
// directive kind it looks like, and the grammar it should have matched.
func malformedLine(lineNo int, line, prefix string) string {
	return fmt.Sprintf("line %d looks like a %q directive but does not parse — expected %q (got %q)",
		lineNo, prefix, directiveGrammar[prefix], line)
}
