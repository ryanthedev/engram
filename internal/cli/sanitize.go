// sanitize.go holds the two body-level safety primitives every vault
// renderer composes. Episodic prose and edge statements are UNTRUSTED
// ingested content whose second use is an Obsidian markdown file — a
// second-use injection surface. sanitizeBody neutralizes markdown/HTML
// constructs that would let prose forge document structure (frontmatter,
// callouts, wikilinks, code fences, raw HTML, dangerous URI schemes);
// quoteBlock wraps already-arbitrary prose into a callout body such that no
// line can exit the quote or forge a nested callout.
//
// Both are transform-not-reject: hostile tokens are defused in place (escape
// or entity substitution) so the surrounding prose stays legible, and both
// are pure and deterministic. quoteBlock does NOT assume its input already
// passed sanitizeBody — each primitive is safe standalone (defense in depth
// on a security path).

package cli

import (
	"regexp"
	"strings"
)

// schemePattern matches the URI schemes that stay dangerous inside a
// markdown link destination even after raw HTML is neutralized:
// obsidian:// (vault-internal actions), javascript: (script execution in
// permissive renderers), data: (content smuggling). \b keeps benign words
// like "metadata:" untouched.
var schemePattern = regexp.MustCompile(`(?i)\b(obsidian|javascript|data):`)

// calloutForgePattern matches a line body that would render as an Obsidian
// callout marker once it sits at the start of a (possibly quoted) line:
// one or more '>' markers followed by "[!". A plain "> quote" is benign and
// deliberately NOT matched.
var calloutForgePattern = regexp.MustCompile(`^(>[ \t]*)*>[ \t]*\[!`)

// sanitizeBody neutralizes untrusted prose for use as a note body. It
// normalizes newlines, drops control characters, and defuses:
//   - raw HTML: every '<' becomes "&lt;" (kills <iframe> beacons, <script>,
//     and HTML blocks; renders back as '<' in preview),
//   - wikilinks/transclusions: adjacent "[[" is broken by escaping the
//     second bracket ("[\["), which also breaks "![[",
//   - dangerous URI schemes: "obsidian:"/"javascript:"/"data:" get a space
//     before the colon, voiding the scheme in any link destination,
//   - line-leading structure forgery: "---" (frontmatter/HR), "```"/"~~~"
//     (fence breakout), and "> [!" (callout forge) are backslash-escaped so
//     they render as literal text.
//
// The transform never drops or truncates the event: benign prose passes
// through unchanged except for the neutralized tokens.
func sanitizeBody(text string) string {
	text = normalizeNewlines(text)
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = escapeLineStart(sanitizeInline(line))
	}
	return strings.Join(lines, "\n")
}

// quoteBlock wraps prose for embedding under a "> [!quote]-" callout: EVERY
// line — blank lines included — is prefixed with "> " so no line can exit
// the callout, and any line that would read as a callout marker after
// prefixing ("[!"-leading, or an already-quoted "> [!") is backslash-escaped
// first so wrapping cannot forge a nested callout.
func quoteBlock(text string) string {
	lines := strings.Split(normalizeNewlines(text), "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		indent := line[:len(line)-len(trimmed)]
		if strings.HasPrefix(trimmed, "[!") || calloutForgeLeading(trimmed) {
			line = indent + `\` + trimmed
		}
		lines[i] = "> " + line
	}
	return strings.Join(lines, "\n")
}

// normalizeNewlines maps CRLF and lone CR to LF so line-start hazard checks
// cannot be dodged with carriage returns.
func normalizeNewlines(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

// sanitizeInline applies the position-independent transforms to one line:
// control characters dropped (tab kept), '<' entity-escaped, "[[" adjacency
// broken, URI schemes defused. The bracket scan tracks the previously
// EMITTED rune — never the previous input rune — so neither pathological
// runs like "[[[[" nor control characters smuggled between brackets
// ("[\x00[") can recombine into an adjacent "[[" after transformation.
func sanitizeInline(line string) string {
	var b strings.Builder
	prevBracket := false
	for _, r := range line {
		switch {
		case r == '\t':
			b.WriteRune('\t')
			prevBracket = false
		case r < 0x20 || r == 0x7f:
			// Drop control characters (including NUL) WITHOUT touching
			// prevBracket: nothing was emitted, so the last emitted rune —
			// possibly '[' — still precedes whatever comes next. Clearing
			// the flag here let "[" + NUL + "[" recombine into a live "[[".
			continue
		case r == '<':
			b.WriteString("&lt;")
			prevBracket = false
		case r == '[':
			if prevBracket {
				b.WriteString(`\[`)
			} else {
				b.WriteRune('[')
			}
			prevBracket = true
		default:
			b.WriteRune(r)
			prevBracket = false
		}
	}
	return schemePattern.ReplaceAllString(b.String(), "${1} :")
}

// escapeLineStart backslash-escapes line-leading structure forgery. Markdown
// gives line-start tokens their power at an indent of at most 3 spaces; at 4+
// the line is already an inert indented code block, so it passes through.
func escapeLineStart(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	indent := line[:len(line)-len(trimmed)]
	if len(indent) > 3 {
		return line
	}
	if strings.HasPrefix(trimmed, "---") ||
		strings.HasPrefix(trimmed, "```") ||
		strings.HasPrefix(trimmed, "~~~") ||
		calloutForgeLeading(trimmed) {
		return indent + `\` + trimmed
	}
	return line
}

// calloutForgeLeading reports whether s begins with a quote-marker run that
// introduces a callout ("> [!", ">[!", "> > [!", ...).
func calloutForgeLeading(s string) bool {
	return calloutForgePattern.MatchString(s)
}
