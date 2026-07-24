package mcp

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// manyHits builds n semantic hits, each padded so the full set overflows a
// small byte budget deterministically.
func manyHits(n int) []Hit {
	hits := make([]Hit, 0, n)
	for i := 0; i < n; i++ {
		hits = append(hits, semanticHit(idOf(i), "s", "p", "o", 50))
	}
	return hits
}

func idOf(i int) string { return "h" + string(rune('a'+i)) }

// globFiles lists the base names of every entry directly inside dir.
func globFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestDW_3_1_SpillWrittenOnlyWhenOmitted: omitted>0 writes a 0600 file
// atomically and returns an absolute overflow_path; omitted==0 writes
// nothing and returns no overflow_path.
func TestDW_3_1_SpillWrittenOnlyWhenOmitted(t *testing.T) {
	t.Run("omitted: file written, overflow_path returned", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(spillDirEnv, dir)
		t.Setenv(searchBudgetBytesEnv, "200")

		hits := manyHits(5)
		_, decoded := searchViaWire(t, &fixedHitsBackend{hits: hits}, map[string]any{"query": "q"})

		omitted, _ := decoded["omitted"].(float64)
		if omitted <= 0 {
			t.Fatalf("omitted = %v, want > 0", decoded["omitted"])
		}
		path, _ := decoded["overflow_path"].(string)
		if path == "" {
			t.Fatalf("overflow_path absent, want a path: %v", decoded)
		}
		if !filepath.IsAbs(path) {
			t.Errorf("overflow_path %q is not absolute", path)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("overflow_path %q not readable: %v", path, err)
		}
		if files := globFiles(t, dir); len(files) != 1 {
			t.Errorf("spill dir has %d entries, want exactly 1: %v", len(files), files)
		}
	})

	t.Run("all fit: no file, no overflow_path", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(spillDirEnv, dir)

		hits := manyHits(2)
		_, decoded := searchViaWire(t, &fixedHitsBackend{hits: hits}, map[string]any{"query": "q"})

		if _, ok := decoded["overflow_path"]; ok {
			t.Errorf("overflow_path present when all hits fit: %v", decoded)
		}
		if files := globFiles(t, dir); len(files) != 0 {
			t.Errorf("spill dir has %d entries, want 0 (no spill when omitted==0): %v", len(files), files)
		}
	})
}

// TestDW_3_2_OverflowPathRoundTrips: reading overflow_path yields valid JSON
// that unmarshals to the FULL slim result set (every hit, none omitted).
func TestDW_3_2_OverflowPathRoundTrips(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(spillDirEnv, dir)
	t.Setenv(searchBudgetBytesEnv, "250")

	hits := manyHits(6)
	_, decoded := searchViaWire(t, &fixedHitsBackend{hits: hits}, map[string]any{"query": "q"})

	path, _ := decoded["overflow_path"].(string)
	if path == "" {
		t.Fatalf("overflow_path absent: %v", decoded)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading overflow_path: %v", err)
	}
	var spilled searchResult[Hit]
	if err := json.Unmarshal(raw, &spilled); err != nil {
		t.Fatalf("overflow_path content is not valid JSON for the result shape: %v (%s)", err, raw)
	}
	if len(spilled.Hits) != len(hits) {
		t.Fatalf("spilled hits = %d, want %d (the FULL slim result set)", len(spilled.Hits), len(hits))
	}
	for i, h := range hits {
		if spilled.Hits[i].ID != h.ID {
			t.Errorf("spilled.Hits[%d].ID = %q, want %q (order-preserving)", i, spilled.Hits[i].ID, h.ID)
		}
	}
	if spilled.Omitted != 0 || spilled.OverflowPath != "" {
		t.Errorf("spilled envelope carries omitted/overflow_path metadata, want a bare full set: %+v", spilled)
	}
}

// TestDW_3_3_SpillFileMode0600: the spill file's mode is exactly 0600,
// asserted via os.Stat, regardless of process umask.
func TestDW_3_3_SpillFileMode0600(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(spillDirEnv, dir)

	path, err := spillFullResult(manyHits(3))
	if err != nil {
		t.Fatalf("spillFullResult: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("os.Stat(%q): %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("spill file mode = %o, want 0600", perm)
	}
}

// captureWarnings swaps slog's default logger for one backed by buf for the
// duration of the test, restoring the original on cleanup.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestDW_3_4_UnwritableSpillDirDegradesGracefully: a permission-denied spill
// dir degrades gracefully — capped response returned, no overflow_path, a
// warning logged, no panic.
func TestDW_3_4_UnwritableSpillDirDegradesGracefully(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil { // read+execute, no write
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) }) // let t.TempDir's own cleanup remove it
	t.Setenv(spillDirEnv, dir)
	t.Setenv(searchBudgetBytesEnv, "200")

	logs := captureWarnings(t)
	hits := manyHits(5)

	text, decoded := searchViaWire(t, &fixedHitsBackend{hits: hits}, map[string]any{"query": "q"}) // must not panic

	omitted, _ := decoded["omitted"].(float64)
	if omitted <= 0 {
		t.Fatalf("omitted = %v, want > 0 (test setup should force overflow): %s", decoded["omitted"], text)
	}
	if _, ok := decoded["overflow_path"]; ok {
		t.Errorf("overflow_path present despite an unwritable spill dir: %v", decoded)
	}
	gotHits, _ := decoded["hits"].([]any)
	if len(gotHits) == 0 {
		t.Errorf("capped response has no hits, want the packed page still returned")
	}
	if !strings.Contains(logs.String(), "spill") || !strings.Contains(logs.String(), "WARN") {
		t.Errorf("no spill warning logged: %q", logs.String())
	}
}

// TestDW_3_5_SpillDirOverridable: ENGRAM_MCP_SPILL_DIR overrides the spill
// directory; unset falls back to os.TempDir(); a nonexistent override
// degrades gracefully like an unwritable one.
func TestDW_3_5_SpillDirOverridable(t *testing.T) {
	t.Run("override used", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(spillDirEnv, dir)
		path, err := spillFullResult(manyHits(2))
		if err != nil {
			t.Fatalf("spillFullResult: %v", err)
		}
		if got := filepath.Dir(path); got != dir {
			t.Errorf("spill file dir = %q, want override %q", got, dir)
		}
	})

	t.Run("unset falls back to os.TempDir", func(t *testing.T) {
		t.Setenv(spillDirEnv, "")
		if got := spillDir(); got != os.TempDir() {
			t.Errorf("spillDir() = %q, want os.TempDir() %q", got, os.TempDir())
		}
	})

	t.Run("nonexistent override degrades gracefully", func(t *testing.T) {
		t.Setenv(spillDirEnv, filepath.Join(t.TempDir(), "does-not-exist"))
		t.Setenv(searchBudgetBytesEnv, "200")
		logs := captureWarnings(t)

		hits := manyHits(5)
		_, decoded := searchViaWire(t, &fixedHitsBackend{hits: hits}, map[string]any{"query": "q"}) // must not panic

		if _, ok := decoded["overflow_path"]; ok {
			t.Errorf("overflow_path present despite a nonexistent spill dir: %v", decoded)
		}
		if !strings.Contains(logs.String(), "spill") {
			t.Errorf("no spill warning logged: %q", logs.String())
		}
	})
}

// TestDW_3_6_MarshalFailureLeavesNoFile: a marshal failure mid-spill (a
// non-finite Score, which encoding/json rejects) leaves NO file created —
// atomicity holds under failure, proven by globbing the spill dir.
func TestDW_3_6_MarshalFailureLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(spillDirEnv, dir)

	hits := []Hit{
		semanticHit("h1", "s", "p", "o", 5),
		{ID: "h2", Score: math.NaN(), Source: "semantic", Fields: `{"statement":"x"}`},
	}

	path, err := spillFullResult(hits)
	if err == nil {
		t.Fatalf("spillFullResult succeeded with a NaN score, want a marshal error (path = %q)", path)
	}
	if path != "" {
		t.Errorf("path = %q on error, want empty", path)
	}
	if files := globFiles(t, dir); len(files) != 0 {
		t.Errorf("spill dir has %d artifacts after a marshal failure, want 0: %v", len(files), files)
	}
}
