package mcp

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// spillDirEnv overrides the directory memory_search spill files are written
// to; unset falls back to the OS temp dir (DW-3.5). Read at the process
// boundary as external input: never validated for existence here — a bad
// directory surfaces as a create failure in spillFullResult, which the
// caller degrades gracefully rather than failing the search.
const spillDirEnv = "ENGRAM_MCP_SPILL_DIR"

// spillTempPattern is the os.CreateTemp name pattern for spill files; the
// ".tmp" suffix marks the file as not-yet-visible until spillFullResult
// renames it into place with that suffix stripped.
const spillTempPattern = "engram-mcp-search-*.json.tmp"

// spillTempSuffix is stripped from the CreateTemp-generated name to derive
// the final (post-rename) spill file name.
const spillTempSuffix = ".tmp"

// spillDir returns the directory memory_search spill files are written to,
// resolved to an absolute path so every path spillFullResult returns is
// absolute regardless of whether the override is relative (DW-3.5).
func spillDir() string {
	dir := os.Getenv(spillDirEnv)
	if dir == "" {
		return os.TempDir() // already absolute
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir // filepath.Abs rarely fails; let CreateTemp surface any real problem
}

// maxRandomSuffixLen is the longest os.CreateTemp's generated random suffix
// can ever be: it formats a uint32 as decimal (see the os package's
// nextRandom), and math.MaxUint32 is the longest such value has to be
// represented in decimal. Derived rather than hand-typed so it can't drift
// from the actual fact it encodes.
var maxRandomSuffixLen = len(strconv.FormatUint(math.MaxUint32, 10))

// maxSpillPath returns the longest absolute path spillFullResult could ever
// return for the CURRENT spillDir(): the fixed prefix/suffix from
// spillTempPattern (with the ".tmp" staging suffix stripped, matching the
// final rename spillFullResult performs) and the random component widened
// to its worst case (maxRandomSuffixLen digits — the real one is that many
// digits or fewer). Building this from the real spillDir() — rather than a
// hardcoded guess — keeps the bound correct even when ENGRAM_MCP_SPILL_DIR
// points somewhere with an unusually long path.
//
// Budget-packing (budget.go) uses this to reserve headroom for the real
// overflow_path field before it exists: the field spillFullResult's actual
// return value will occupy is guaranteed to be no longer than this one.
func maxSpillPath() string {
	finalPattern := strings.TrimSuffix(spillTempPattern, spillTempSuffix)
	worstCaseName := strings.Replace(finalPattern, "*", strings.Repeat("9", maxRandomSuffixLen), 1)
	return filepath.Join(spillDir(), worstCaseName)
}

// spillFullResult atomically writes the full slim result set (every hit,
// none omitted) to a private, owner-only scratch file in spillDir(),
// returning its absolute path.
//
// It mirrors internal/cli/export.go's writeFileAtomic (CreateTemp + Rename)
// so a crash or error at any step never leaves a partial or world-readable
// file behind: content is marshaled to bytes *before* any filesystem call
// (a marshal failure — e.g. a non-finite Score — never touches disk at
// all), and every error branch after that removes the temp file before
// returning (DW-3.6). Callers must treat a non-nil error as "spill did not
// happen" and degrade gracefully — this function never partially succeeds.
func spillFullResult[H packable](hits []H) (path string, err error) {
	content, err := json.Marshal(searchResult[H]{Hits: hits})
	if err != nil {
		return "", fmt.Errorf("mcp: marshaling spill content: %w", err)
	}

	dir := spillDir()
	tmp, err := os.CreateTemp(dir, spillTempPattern)
	if err != nil {
		return "", fmt.Errorf("mcp: creating spill file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()

	// The file mode the spill content may contain sensitive memory text, so
	// pin 0600 explicitly rather than relying on os.CreateTemp's default
	// (which is already 0600, but the process umask is not this code's to
	// trust).
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("mcp: setting spill file mode: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("mcp: writing spill file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("mcp: closing spill file: %w", err)
	}

	finalName := strings.TrimSuffix(tmpName, spillTempSuffix)
	if err := os.Rename(tmpName, finalName); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("mcp: renaming spill file into place: %w", err)
	}
	return finalName, nil
}
