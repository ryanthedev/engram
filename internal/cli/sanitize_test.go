package cli

// White-box tests for the two body-level safety primitives (Phase 2,
// security-sensitive). The DW-2.1 corpus is adversarial-heavy (≥5:1
// dirty:benign) and every output — dirty or benign — is swept by the same
// hazard detectors, so a neutralization that merely relocates a hazard
// still fails.

import (
	"regexp"
	"strings"
	"testing"
)

// hazardDetectors flag any output that still contains an active injection
// token. They run in multiline mode over whole sanitized bodies.
var hazardDetectors = []struct {
	name string
	re   *regexp.Regexp
}{
	{"line-leading ---", regexp.MustCompile(`(?m)^ {0,3}---`)},
	{"line-leading fence", regexp.MustCompile("(?m)^ {0,3}(`{3}|~{3})")},
	{"callout forge", regexp.MustCompile(`(?m)^ {0,3}(>[ \t]*)+\[!`)},
	{"adjacent [[", regexp.MustCompile(`\[\[`)},
	{"raw html <", regexp.MustCompile(`<`)},
	{"dangerous scheme", regexp.MustCompile(`(?i)\b(obsidian|javascript|data):`)},
}

func assertNoHazards(t *testing.T, out string) {
	t.Helper()
	for _, d := range hazardDetectors {
		if loc := d.re.FindString(out); loc != "" {
			t.Errorf("hazard %q survived sanitization: %q in output %q", d.name, loc, out)
		}
	}
}

func TestDW_2_1_SanitizeBodyAdversarialCorpus(t *testing.T) {
	// 18 dirty vs 3 benign cases (6:1). Each case asserts the specific
	// neutralization AND runs the full hazard sweep; `keep` substrings prove
	// the prose stayed legible (transform-not-reject — never dropped).
	tests := []struct {
		name string
		in   string
		keep []string // substrings that must survive in the output
	}{
		// -- dirty --
		{"leading frontmatter forge", "---\ntitle: forged\n---\nreal prose", []string{"title: forged", "real prose", `\---`}},
		{"mid-body hr/frontmatter line", "para one\n---\npara two", []string{"para one", `\---`, "para two"}},
		{"indented frontmatter dodge", "  ---\nx", []string{`  \---`}},
		{"callout forge", "> [!danger] pwned\nafter", []string{`\> [!danger] pwned`, "after"}},
		{"callout forge no space", ">[!note]tight", []string{`\>[!note]tight`}},
		{"nested callout forge", "> > [!tip] deep", []string{`\> > [!tip] deep`}},
		{"plain quote stays a quote", "> just a quote", []string{"> just a quote"}},
		{"wikilink", "see [[Secret Note]] now", []string{`[\[Secret Note]]`, "now"}},
		{"transclusion", "![[embed.png]]", []string{`![\[embed.png]]`}},
		{"bracket run cannot recombine", "[[[[deep", []string{"deep"}},
		{"NUL between brackets cannot recombine", "[\x00[link]]", []string{`[\[link]]`}},
		{"NUL-split transclusion cannot recombine", "![\x00[img]]", []string{`![\[img]]`}},
		{"DEL between brackets cannot recombine", "a[\x7f[b", []string{`a[\[b`}},
		{"multiple control chars between brackets", "[\x01\x02\x1f[x", []string{`[\[x`}},
		{"backtick fence breakout", "```bash\nrm -rf /\n```", []string{"\\```bash", "rm -rf /"}},
		{"tilde fence breakout", "~~~\nx\n~~~", []string{`\~~~`}},
		{"iframe beacon", `<iframe src="http://evil.example"></iframe>`, []string{"&lt;iframe", "evil.example"}},
		{"script tag", "<script>alert(1)</script>", []string{"&lt;script"}},
		{"javascript scheme", "[click](javascript:alert(1))", []string{"javascript :alert(1)"}},
		{"obsidian scheme", "[open](obsidian://vault?x=1)", []string{"obsidian ://vault"}},
		{"data scheme mixed case", "[i](DaTa:text/html;base64,AAAA)", []string{"DaTa :text/html"}},
		{"cr dodge on line hazards", "safe\r---\r> [!x] hi", []string{"safe", `\---`, `\> [!x] hi`}},
		// -- benign --
		{"plain prose untouched", "Alice met Bob to discuss the roadmap.\n\nThey agreed on Tuesday.", []string{"Alice met Bob to discuss the roadmap.", "They agreed on Tuesday."}},
		{"single brackets and https link untouched", "see [the doc](https://example.com/a) and [note] items", []string{"see [the doc](https://example.com/a) and [note] items"}},
		{"metadata: is not the data: scheme", "the metadata: field is set", []string{"the metadata: field is set"}},
		{"tab preserved unlike other control chars", "col1\tcol2", []string{"col1\tcol2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := sanitizeBody(tt.in)
			assertNoHazards(t, out)
			for _, want := range tt.keep {
				if !strings.Contains(out, want) {
					t.Errorf("sanitizeBody(%q) = %q, want it to keep %q", tt.in, out, want)
				}
			}
			if out == "" && tt.in != "" {
				t.Errorf("sanitizeBody(%q) dropped the whole input", tt.in)
			}
		})
	}
}

func TestDW_2_1_SanitizeBodyBenignProseIntact(t *testing.T) {
	// Fully benign multi-line prose must round-trip byte-identically.
	in := "First paragraph with a [ref] and a dash - here.\n\nSecond paragraph.\n> a genuine quote\n    indented code line"
	if out := sanitizeBody(in); out != in {
		t.Errorf("benign prose changed:\n in: %q\nout: %q", in, out)
	}
}

func TestDW_2_1_SanitizeBodyNoRecombination(t *testing.T) {
	// Bracket runs of every length must yield zero adjacent "[[" — the
	// classic failure of single-pass ReplaceAll("[[", ...).
	inputs := []string{
		"[[", "[[[", "[[[[", "[[[[[", "a[[b[[[c",
		// Dropped control characters emit nothing, so they must not clear
		// the last-EMITTED-rune state (the reviewed recombination bypass).
		"[\x00[link]]", "![\x00[img]]", "a[\x7f[b", "[\x00[\x00[", "[\x01\x02\x1f[x",
	}
	for _, in := range inputs {
		out := sanitizeBody(in)
		if strings.Contains(out, "[[") {
			t.Errorf("sanitizeBody(%q) = %q still contains adjacent [[", in, out)
		}
	}
}

func TestDW_2_2_QuoteBlockPrefixesEveryLine(t *testing.T) {
	in := "first line\n\nthird line after blank\n  indented"
	out := quoteBlock(in)
	lines := strings.Split(out, "\n")
	if len(lines) != 4 {
		t.Fatalf("quoteBlock changed line count: got %d lines %q", len(lines), out)
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, "> ") {
			t.Errorf("line %d %q lacks the \"> \" prefix", i, line)
		}
	}
	if lines[1] != "> " {
		t.Errorf("blank line rendered %q, want \"> \" (an unprefixed blank exits the callout)", lines[1])
	}
	if quoteBlock("") != "> " {
		t.Errorf("quoteBlock(\"\") = %q, want \"> \"", quoteBlock(""))
	}
}

func TestDW_2_2_QuoteBlockCannotForgeOrExitCallout(t *testing.T) {
	// After wrapping, no line may (a) lack the quote prefix (exit) or
	// (b) read as a callout marker at any quote depth (forge).
	forge := regexp.MustCompile(`^(>[ \t]*)+\[!`)
	tests := []struct {
		name string
		in   string
	}{
		{"callout-leading line", "[!danger] you have been pwned"},
		{"indented callout lead", "  [!note] sneaky"},
		{"already-quoted callout", "> [!tip] nested forge"},
		{"deeply quoted callout", "> > [!x] deeper"},
		{"quote marker without space", ">[!x]tight"},
		{"blank then callout", "\n[!warn] after blank"},
		{"cr smuggled lines", "ok\r[!x] cr-split\r\n> [!y] crlf-split"},
		{"benign multi-line", "just prose\nmore prose"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := quoteBlock(tt.in)
			for i, line := range strings.Split(out, "\n") {
				if !strings.HasPrefix(line, "> ") {
					t.Errorf("line %d %q escaped the quote prefix", i, line)
				}
				if forge.MatchString(line) {
					t.Errorf("line %d %q forges a callout marker", i, line)
				}
			}
		})
	}
}

func TestQuoteBlockOfSanitizedBodyComposes(t *testing.T) {
	// The renderer composition quoteBlock(sanitizeBody(x)) — the Phase 3
	// hot path — must satisfy both contracts at once on hostile input.
	in := "---\n> [!danger] a\n[[link]]\n\n<script>x</script>\njavascript:go()"
	out := quoteBlock(sanitizeBody(in))
	forge := regexp.MustCompile(`^(>[ \t]*)+\[!`)
	for i, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "> ") {
			t.Errorf("line %d %q escaped the quote", i, line)
		}
		if forge.MatchString(line) {
			t.Errorf("line %d %q forges a callout", i, line)
		}
	}
	if strings.Contains(out, "[[") || strings.Contains(out, "<script") {
		t.Errorf("composition leaked a hazard: %q", out)
	}
}
