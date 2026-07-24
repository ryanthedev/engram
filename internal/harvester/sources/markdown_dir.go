package sources

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryanthedev/engram/internal/harvester"
	"github.com/ryanthedev/engram/internal/mcp"
	yaml "go.yaml.in/yaml/v2"
)

// markdownSource harvests one document per markdown file from one or more local
// directories of notes ("brains"), parsing each file's YAML frontmatter into the
// collection's declared fields.
//
// Unlike github-repos there is no clone and no commit SHA, so source_version is
// a content hash of the raw file bytes: a byte-identical file re-harvests to the
// same version, an edited file to a new one.
type markdownSource struct {
	roots        []noteRoot
	files        []string
	exclude      []string
	maxFileBytes int64
	deps         harvester.Deps
}

// noteRoot is one configured directory of notes. path is always cleaned and
// absolute (resolved at construction, including `~` expansion) so the sweep
// scope derived from it is stable across working directories.
type noteRoot struct {
	path  string
	brain string
}

var _ harvester.Source = (*markdownSource)(nil)
var _ harvester.ScopedSource = (*markdownSource)(nil)

// markdownScopePrefix namespaces markdown-dir sweep scopes. A per-root scope is
// markdownScopePrefix + the cleaned absolute root path, giving each configured
// root its OWN mark-and-sweep scope so harvesting one brain in a separate run
// never deletes another brain's documents.
const markdownScopePrefix = "markdown-dir:"

// defaultNoteGlobs is the `files` default: every markdown file at any depth.
var defaultNoteGlobs = []string{"**/*.md"}

func init() {
	harvester.Register("markdown-dir", func(cfg harvester.SourceConfig, deps harvester.Deps) (harvester.Source, error) {
		rootsVal, ok := cfg.Raw["roots"]
		if !ok {
			return nil, fmt.Errorf("harvester: markdown-dir: missing required config 'roots'")
		}
		roots, err := parseNoteRoots(rootsVal)
		if err != nil {
			return nil, fmt.Errorf("harvester: markdown-dir: invalid 'roots' config: %w", err)
		}
		if len(roots) == 0 {
			return nil, fmt.Errorf("harvester: markdown-dir: 'roots' list cannot be empty")
		}

		files := defaultNoteGlobs
		if fVal, ok := cfg.Raw["files"]; ok {
			parsed, err := parseStringSlice(fVal)
			if err != nil {
				return nil, fmt.Errorf("harvester: markdown-dir: invalid 'files' config: %w", err)
			}
			if len(parsed) > 0 {
				files = parsed
			}
		}

		var exclude []string
		if eVal, ok := cfg.Raw["exclude"]; ok {
			parsed, err := parseStringSlice(eVal)
			if err != nil {
				return nil, fmt.Errorf("harvester: markdown-dir: invalid 'exclude' config: %w", err)
			}
			exclude = parsed
		}

		maxFileBytes := int64(1 << 20)
		if mVal, ok := cfg.Raw["max_file_bytes"]; ok {
			switch val := mVal.(type) {
			case int:
				maxFileBytes = int64(val)
			case int64:
				maxFileBytes = val
			case float64:
				maxFileBytes = int64(val)
			default:
				return nil, fmt.Errorf("harvester: markdown-dir: 'max_file_bytes' must be an integer, got %T", mVal)
			}
		}

		return &markdownSource{
			roots:        roots,
			files:        files,
			exclude:      exclude,
			maxFileBytes: maxFileBytes,
			deps:         deps,
		}, nil
	})
}

// Type returns the source type name.
func (s *markdownSource) Type() string {
	return "markdown-dir"
}

// Mode returns FullHarvest: a deleted note is removed by the Runner's sweep of
// the owning root's scope, exactly as for github-repos.
func (s *markdownSource) Mode() harvester.HarvestMode {
	return harvester.FullHarvest
}

// SweepScopes returns one sweep scope per configured root (deduplicated),
// derived from CONFIG so a root that yields zero documents this run still has
// its own stale docs swept and nothing orphans.
//
// Known, deliberate edge case (mirroring github-repos' per-repo scoping): two
// entries pointing at the SAME directory under different brain names collapse
// to ONE scope, so the second entry's ingest would sweep the first's docs. Give
// each brain its own directory, or list a directory once.
func (s *markdownSource) SweepScopes() []string {
	seen := make(map[string]bool, len(s.roots))
	scopes := make([]string, 0, len(s.roots))
	for _, r := range s.roots {
		scope := markdownScopePrefix + r.path
		if seen[scope] {
			continue
		}
		seen[scope] = true
		scopes = append(scopes, scope)
	}
	return scopes
}

// HarvestScope harvests only the root(s) belonging to scope into sink. scope is
// always one of SweepScopes(); roots not matching the scope are skipped.
func (s *markdownSource) HarvestScope(ctx context.Context, scope string, sink harvester.Sink) error {
	for _, root := range s.roots {
		if markdownScopePrefix+root.path != scope {
			continue
		}
		if err := s.harvestRoot(ctx, root, sink); err != nil {
			return err
		}
	}
	return nil
}

// Harvest walks every configured root and adds glob-matching markdown files to
// the sink. This is the single-scope interpretation (all roots under
// source=Type()); the Runner drives markdown-dir per-scope via ScopedSource
// instead, so each root is swept independently. Retained for the plain Source
// contract and direct use.
func (s *markdownSource) Harvest(ctx context.Context, sink harvester.Sink) error {
	for _, root := range s.roots {
		if err := s.harvestRoot(ctx, root, sink); err != nil {
			return err
		}
	}
	return nil
}

// harvestRoot walks one root and adds its glob-matching notes to sink.
//
// A root that does not exist (or is not a directory) is a hard error rather than
// an empty harvest: an empty harvest of a Full-Harvest scope would let the
// Runner's sweep delete every document that root ever contributed, so a typo'd
// or unmounted path must abort the run before the sweep.
func (s *markdownSource) harvestRoot(ctx context.Context, root noteRoot, sink harvester.Sink) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("harvester: markdown-dir: cancelled: %w", err)
	}

	info, err := os.Stat(root.path)
	if err != nil {
		return fmt.Errorf("harvester: markdown-dir: stat root %q: %w", root.path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("harvester: markdown-dir: root %q is not a directory", root.path)
	}

	globMatchCount := make(map[string]int, len(s.files))
	for _, pattern := range s.files {
		globMatchCount[pattern] = 0
	}

	err = filepath.WalkDir(root.path, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cErr := ctx.Err(); cErr != nil {
			return cErr
		}

		relPath, err := filepath.Rel(root.path, path)
		if err != nil {
			return fmt.Errorf("getting relative path for %s: %w", path, err)
		}
		relPath = filepath.ToSlash(relPath)

		if d.IsDir() {
			if relPath == "." {
				return nil
			}
			// .git is never notes, and an excluded subtree is pruned here so a
			// large .trash/ is never walked at all.
			if d.Name() == ".git" || s.isExcluded(ctx, relPath) {
				return filepath.SkipDir
			}
			return nil
		}

		// Symlinks are skipped rather than followed: a link out of the root would
		// harvest documents whose path is not root-relative (and can loop).
		if d.Type()&os.ModeSymlink != 0 {
			s.deps.Logger.WarnContext(ctx, "harvester: markdown-dir: skipping symlink",
				slog.String("brain", root.brain),
				slog.String("path", relPath),
			)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		if s.isExcluded(ctx, relPath) {
			return nil
		}

		matchedAny := false
		for _, pattern := range s.files {
			matched, err := matchGlob(pattern, relPath)
			if err != nil {
				s.deps.Logger.WarnContext(ctx, "harvester: markdown-dir: invalid glob pattern",
					slog.String("pattern", pattern),
					slog.String("error", err.Error()),
				)
				continue
			}
			if matched {
				matchedAny = true
				globMatchCount[pattern]++
			}
		}
		if !matchedAny {
			return nil
		}

		fileInfo, err := d.Info()
		if err != nil {
			return fmt.Errorf("getting info for %s: %w", path, err)
		}
		if fileInfo.Size() > s.maxFileBytes {
			s.deps.Logger.WarnContext(ctx, "harvester: markdown-dir: skipping oversized file",
				slog.String("brain", root.brain),
				slog.String("path", relPath),
				slog.Int64("size", fileInfo.Size()),
				slog.Int64("max_bytes", s.maxFileBytes),
			)
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading file %s: %w", path, err)
		}
		if int64(len(data)) > s.maxFileBytes {
			s.deps.Logger.WarnContext(ctx, "harvester: markdown-dir: skipping oversized file after read",
				slog.String("brain", root.brain),
				slog.String("path", relPath),
			)
			return nil
		}
		if bytes.IndexByte(data, 0) != -1 {
			s.deps.Logger.WarnContext(ctx, "harvester: markdown-dir: skipping binary file",
				slog.String("brain", root.brain),
				slog.String("path", relPath),
			)
			return nil
		}

		if err := sink.Add(s.buildDoc(ctx, root, relPath, data)); err != nil {
			return fmt.Errorf("adding doc to sink: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("harvester: markdown-dir: walking notes under %q: %w", root.path, err)
	}

	for pattern, count := range globMatchCount {
		if count == 0 {
			s.deps.Logger.WarnContext(ctx, "harvester: markdown-dir: glob matched zero files",
				slog.String("brain", root.brain),
				slog.String("pattern", pattern),
			)
		}
	}
	return nil
}

// buildDoc turns one note's raw bytes into the KnowledgeDoc the `notes`
// collection declares. The field set is exhaustive and deliberate: the knowledge
// index is dynamic:strict, so an undeclared (or empty-but-typed, e.g. a blank
// `date`) field is rejected per-item, which surfaces as ErrPartialIngest and
// aborts the whole run before the sweep. Optional frontmatter keys are therefore
// OMITTED, never emitted empty.
func (s *markdownSource) buildDoc(ctx context.Context, root noteRoot, relPath string, data []byte) mcp.KnowledgeDoc {
	front, body := splitFrontmatter(ctx, s.deps.Logger, relPath, data)

	title := relPath
	if name, ok := frontmatterString(front["name"]); ok {
		title = name
	}

	sum := sha256.Sum256(data)
	fields := map[string]any{
		"brain": root.brain,
		"path":  relPath,
	}
	// category is the containing subdirectory; a note sitting directly in the
	// root has none, and omitting beats inventing a placeholder.
	if idx := strings.Index(relPath, "/"); idx > 0 {
		fields["category"] = relPath[:idx]
	}
	if noteType, ok := frontmatterString(front["type"]); ok {
		fields["note_type"] = noteType
	}
	if date, ok := normalizeFrontmatterDate(front["date"]); ok {
		fields["date"] = date
	} else if _, present := front["date"]; present {
		s.deps.Logger.WarnContext(ctx, "harvester: markdown-dir: omitting unparseable frontmatter date",
			slog.String("brain", root.brain),
			slog.String("path", relPath),
		)
	}
	if description, ok := frontmatterString(front["description"]); ok {
		fields["description"] = description
	}

	return mcp.KnowledgeDoc{
		ID:            root.brain + "/" + relPath,
		Title:         title,
		Text:          body,
		SourceVersion: "sha256:" + hex.EncodeToString(sum[:])[:16],
		Fields:        fields,
	}
}

// isExcluded reports whether the root-relative path is skipped by any `exclude`
// pattern. An invalid pattern is logged and treated as non-matching rather than
// failing the run.
func (s *markdownSource) isExcluded(ctx context.Context, relPath string) bool {
	for _, pattern := range s.exclude {
		matched, err := matchExcludeGlob(pattern, relPath)
		if err != nil {
			s.deps.Logger.WarnContext(ctx, "harvester: markdown-dir: invalid exclude pattern",
				slog.String("pattern", pattern),
				slog.String("error", err.Error()),
			)
			continue
		}
		if matched {
			return true
		}
	}
	return false
}

// matchExcludeGlob matches one `exclude` pattern against a root-relative path.
//
// It is matchGlob plus SUBTREE semantics for a trailing "/**": matchGlob's `**`
// never crosses a `/`, so a bare path.Match of ".trash/**" would match
// ".trash/note.md" but NOT ".trash/2024/note.md" — useless for excluding a
// directory. A pattern ending in "/**" therefore matches the prefix directory
// itself and everything beneath it at any depth (which also lets the walk prune
// the directory). Every other pattern keeps github-repos' `files` semantics.
func matchExcludeGlob(pattern, relPath string) (bool, error) {
	if prefix, ok := strings.CutSuffix(pattern, "/**"); ok && prefix != "" {
		for cur := relPath; ; {
			matched, err := matchGlob(prefix, cur)
			if err != nil {
				return false, err
			}
			if matched {
				return true, nil
			}
			idx := strings.LastIndex(cur, "/")
			if idx < 0 {
				break
			}
			cur = cur[:idx]
		}
	}
	return matchGlob(pattern, relPath)
}

// splitFrontmatter separates a leading YAML frontmatter block (a `---` line, the
// YAML, and a closing `---` or `...` line) from the note body, returning the
// parsed keys and the body with the block stripped.
//
// Notes in the wild are messy, in two distinct ways that must NOT be conflated:
//
//   - No frontmatter block at all (no leading `---`, or an unterminated block).
//     There is nothing to strip, so the WHOLE file is the body and there are no
//     keys.
//   - A frontmatter block that is present and correctly delimited but whose YAML
//     does not parse. The block's extent is known, so the body is still the text
//     AFTER it, and the keys are recovered leniently (see recoverFrontmatter).
//     Indexing the raw `---` fence and its YAML as prose helps nobody.
//
// Neither may abort a run.
func splitFrontmatter(ctx context.Context, logger *slog.Logger, relPath string, data []byte) (map[string]any, string) {
	text := strings.TrimPrefix(string(data), "\ufeff")

	rest, ok := cutDelimiterLine(text)
	if !ok {
		return nil, text
	}

	block, body, ok := cutFrontmatterBlock(rest)
	if !ok {
		return nil, text
	}

	var raw map[interface{}]interface{}
	if err := yaml.Unmarshal([]byte(block), &raw); err != nil {
		recovered := recoverFrontmatter(block)
		if len(recovered) == 0 {
			logger.WarnContext(ctx, "harvester: markdown-dir: unparseable frontmatter, no metadata recovered",
				slog.String("path", relPath),
				slog.String("error", err.Error()),
			)
			return nil, body
		}
		logger.InfoContext(ctx, "harvester: markdown-dir: unparseable frontmatter, recovered keys leniently",
			slog.String("path", relPath),
			slog.String("error", err.Error()),
			slog.Int("recovered_keys", len(recovered)),
		)
		return recovered, body
	}
	return cleanFrontmatterMap(raw), body
}

// recoverableFrontmatterKeys is the closed set of frontmatter keys the lenient
// fallback will recover — exactly the keys buildDoc reads (`name` for the title,
// `type` for note_type, plus `date` and `description`). It is deliberately a
// whitelist rather than "every line that looks like a key": the fallback runs on
// YAML we already know is broken, so an arbitrary key/value harvested out of a
// malformed block is more likely to be a fragment of a mangled value than real
// metadata, and the knowledge index is dynamic:strict anyway — an unexpected key
// would be dropped by buildDoc regardless.
var recoverableFrontmatterKeys = map[string]bool{
	"name":        true,
	"type":        true,
	"date":        true,
	"description": true,
}

// recoverFrontmatter salvages known scalar keys from a frontmatter block that
// yaml.Unmarshal rejected, returning only the keys it could recover (possibly
// none). Values are always plain strings, so the result is structpb-encodable by
// construction and `date` still routes through normalizeFrontmatterDate in
// buildDoc exactly as the strict path's value does.
//
// Every real-world failure observed on this corpus is the same YAML rule biting
// a human-written scalar: an unquoted value containing ": " (`name: Raw
// research: Managed Agents`) ends the scalar and starts a nested mapping, which
// YAML forbids there. The recovery is therefore deliberately dumber than YAML:
//
//   - The candidate key is the text before the FIRST colon; the value is
//     EVERYTHING after it, further colons included. That is the whole point —
//     `name: Raw research: Managed Agents` must recover the full title.
//   - The line must start at column 0. A leading space or tab means the line is
//     an indented continuation, a nested mapping's child, or a list item that
//     belongs to the PREVIOUS key's value; consuming it would attribute another
//     key's text to a top-level field. This is also what keeps a `Fix: ...`
//     fragment sitting inside a folded value from being mistaken for a key —
//     that plus the whitelist above.
//   - A key seen more than once keeps the FIRST occurrence carrying a NON-EMPTY
//     value. Note this DIVERGES from yaml v2, which accepts a well-formed
//     duplicate key and keeps the LAST occurrence. First-wins is the right rule
//     here because the second "occurrence" in a broken block is usually not a
//     real duplicate at all — it is a fragment of the first key's mangled value
//     that happens to start with the same word. Preferring the earlier, more
//     likely-intact reading loses less than trusting the debris after it.
//   - An EMPTY (or whitespace-only) value never claims the key. A bare `type:`
//     sitting above a real `type: gotcha` would otherwise take the slot under
//     first-wins and drop the only usable value in the block. Nothing is lost by
//     skipping it — buildDoc omits an empty scalar anyway — and it keeps the
//     recovered-key COUNT honest, so a block whose only known keys are empty is
//     still reported as recovering nothing.
func recoverFrontmatter(block string) map[string]any {
	out := make(map[string]any, len(recoverableFrontmatterKeys))
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		// Column-0 requirement. An empty line has no key either.
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		rawKey, rawValue, hasColon := strings.Cut(line, ":")
		if !hasColon {
			continue
		}
		// Only trailing space is possible (column 0 is guaranteed non-blank), but
		// `name : value` is a spelling humans produce, so tolerate it.
		key := strings.TrimRight(rawKey, " \t")
		if !recoverableFrontmatterKeys[key] {
			continue
		}
		// Only non-empty values are ever stored, so a hit here is a real
		// first-wins collision rather than a slot held open by a bare `key:`.
		if _, alreadySeen := out[key]; alreadySeen {
			continue
		}
		value := unquoteFrontmatterScalar(strings.TrimSpace(rawValue))
		if value == "" {
			continue
		}
		out[key] = value
	}
	return out
}

// unquoteFrontmatterScalar strips ONE layer of matching surrounding quotes so a
// well-quoted value inside an otherwise-broken block reads the same as it would
// have via YAML.
//
// The rule is exactly "first byte and last byte are the SAME quote character,
// and there are at least two bytes". Everything else is returned VERBATIM,
// quote characters included — never half-stripped, never dropped:
//
//   - Opening quote, prose after it: the motivating real note is
//     `description: "Be concise" can't control a reasoning model's token count`.
//     The leading quote is not a quoting construct at all, it is a quotation
//     inside the prose. Keeping it is lossless and closer to the author's intent
//     than guessing where the string was meant to end.
//   - Mismatched pair (`'value"`): two different quote characters are not a
//     wrapper, so neither is removed.
//   - A LONE quote character as the whole value. The len<2 guard is what makes
//     this safe: a single `"` has the same byte at the first and last position,
//     so a naive first-and-last test would "strip" it into the empty string and
//     the field would then be omitted by frontmatterString — silently losing the
//     only content the line had. A pair needs two bytes to be a pair.
//
// No escape processing is attempted — this is a salvage path, not a YAML
// implementation. A value that reaches here already failed a real YAML parse.
func unquoteFrontmatterScalar(value string) string {
	if len(value) < 2 {
		return value
	}
	quote := value[0]
	if (quote == '"' || quote == '\'') && value[len(value)-1] == quote {
		return value[1 : len(value)-1]
	}
	return value
}

// cutDelimiterLine consumes a leading `---` line and returns the remainder.
func cutDelimiterLine(text string) (string, bool) {
	line, rest, hadNewline := strings.Cut(text, "\n")
	if !hadNewline || strings.TrimRight(line, "\r") != "---" {
		return "", false
	}
	return rest, true
}

// cutFrontmatterBlock splits the YAML block from the body at the first closing
// `---` or `...` line. An unterminated block reports false.
func cutFrontmatterBlock(rest string) (block, body string, ok bool) {
	offset := 0
	for offset <= len(rest) {
		line, remainder, hadNewline := strings.Cut(rest[offset:], "\n")
		switch strings.TrimRight(line, "\r") {
		case "---", "...":
			return rest[:offset], remainder, true
		}
		if !hadNewline {
			return "", "", false
		}
		offset = len(rest) - len(remainder)
	}
	return "", "", false
}

// cleanFrontmatterMap mirrors the manifest loader's cleanYAMLMap (unexported in
// package harvester): yaml v2 decodes nested mappings as
// map[interface{}]interface{}, which structpb.NewStruct REJECTS outright, so any
// value that could reach KnowledgeDoc.Fields is normalized to map[string]any
// first. Frontmatter is arbitrary user YAML, so this runs on every note.
func cleanFrontmatterMap(m map[interface{}]interface{}) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		strKey, ok := k.(string)
		if !ok {
			strKey = fmt.Sprintf("%v", k)
		}
		out[strKey] = cleanFrontmatterVal(v)
	}
	return out
}

func cleanFrontmatterVal(v any) any {
	switch val := v.(type) {
	case map[interface{}]interface{}:
		return cleanFrontmatterMap(val)
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, item := range val {
			out[k] = cleanFrontmatterVal(item)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = cleanFrontmatterVal(item)
		}
		return out
	default:
		return v
	}
}

// frontmatterString reads a scalar frontmatter value as a non-empty string.
// Non-string scalars (an unquoted number or bool) are formatted rather than
// dropped; composite values and empty strings report false so the caller omits
// the field instead of emitting an empty or wrongly-typed one.
func frontmatterString(v any) (string, bool) {
	switch val := v.(type) {
	case nil:
		return "", false
	case string:
		trimmed := strings.TrimSpace(val)
		return trimmed, trimmed != ""
	case bool, int, int64, uint64, float64:
		return fmt.Sprintf("%v", val), true
	default:
		return "", false
	}
}

// frontmatterDateLayouts are the accepted `date:` spellings, in try order.
var frontmatterDateLayouts = []string{
	"2006-01-02",
	"2006-1-2",
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006/01/02",
}

// normalizeFrontmatterDate renders a frontmatter `date` as a YYYY-MM-DD STRING,
// reporting false when the value cannot be read as a date.
//
// Two traps meet here. First, yaml v2 resolves an UNQUOTED `date: 2026-06-02`
// to a time.Time (its allowedTimestampFormats include the date-only form), and
// structpb.NewStruct rejects a time.Time outright — that failure is not
// per-field, it fails the whole ingest batch. Second, the `date` field is a
// typed mapping, so a note carrying a malformed value must yield NO date field
// at all rather than an empty or garbage string.
func normalizeFrontmatterDate(v any) (string, bool) {
	switch val := v.(type) {
	case time.Time:
		return val.Format("2006-01-02"), true
	case string:
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return "", false
		}
		for _, layout := range frontmatterDateLayouts {
			if parsed, err := time.Parse(layout, trimmed); err == nil {
				return parsed.Format("2006-01-02"), true
			}
		}
		return "", false
	default:
		return "", false
	}
}

// parseNoteRoots parses the `roots` config: each entry is either a directory
// path string or an object with `path` (required) and `brain` (optional,
// defaulting to the directory's base name).
func parseNoteRoots(val any) ([]noteRoot, error) {
	if val == nil {
		return nil, nil
	}

	var values []any
	switch slice := val.(type) {
	case []string:
		values = make([]any, len(slice))
		for i, path := range slice {
			values[i] = path
		}
	case []any:
		values = slice
	default:
		return nil, fmt.Errorf("expected slice, got %T", val)
	}

	roots := make([]noteRoot, 0, len(values))
	for i, value := range values {
		var rawPath, brain string
		switch item := value.(type) {
		case string:
			rawPath = item
		case map[string]any:
			pathValue, ok := item["path"]
			if !ok {
				return nil, fmt.Errorf("element at index %d is missing required key 'path'", i)
			}
			var pathOK bool
			rawPath, pathOK = pathValue.(string)
			if !pathOK {
				return nil, fmt.Errorf("element at index %d key 'path' must be a string, got %T", i, pathValue)
			}
			if brainValue, ok := item["brain"]; ok {
				var brainOK bool
				brain, brainOK = brainValue.(string)
				if !brainOK {
					return nil, fmt.Errorf("element at index %d key 'brain' must be a string, got %T", i, brainValue)
				}
			}
		default:
			return nil, fmt.Errorf("element at index %d must be a string or map, got %T", i, value)
		}

		cleaned, err := resolveRootPath(rawPath)
		if err != nil {
			return nil, fmt.Errorf("element at index %d: %w", i, err)
		}
		if brain == "" {
			brain = filepath.Base(cleaned)
		}
		if err := validateBrain(brain); err != nil {
			return nil, fmt.Errorf("element at index %d: %w", i, err)
		}
		roots = append(roots, noteRoot{path: cleaned, brain: brain})
	}
	return roots, nil
}

// resolveRootPath expands a leading `~`, makes the path absolute and cleans it.
// Nothing else in the harvester expands `~`, so it is done here rather than
// leaving a literal "~/notes" directory to be created by mistake; the resulting
// absolute path is also what the root's sweep scope is built from, so it must
// not depend on the process working directory.
func resolveRootPath(rawPath string) (string, error) {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return "", fmt.Errorf("root path cannot be empty")
	}
	if trimmed == "~" || strings.HasPrefix(trimmed, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding %q: %w", trimmed, err)
		}
		trimmed = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(trimmed, "~"), "/"))
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolving root path %q: %w", rawPath, err)
	}
	return filepath.Clean(abs), nil
}

// validateBrain guards the brain name: it is the first segment of every document
// id this root emits, so it must be non-empty and carry no path separator.
func validateBrain(brain string) error {
	if strings.TrimSpace(brain) == "" {
		return fmt.Errorf("brain name cannot be empty")
	}
	if strings.ContainsAny(brain, `/\`) {
		return fmt.Errorf("brain name %q cannot contain a path separator", brain)
	}
	if brain == "." || brain == ".." {
		return fmt.Errorf("brain name %q is not a valid identifier", brain)
	}
	return nil
}
