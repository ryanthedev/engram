package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
func spillFullResult(hits []Hit) (path string, err error) {
	content, err := json.Marshal(searchResult{Hits: hits})
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
