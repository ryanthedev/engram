// Package testutil holds small helpers shared by tests only.
package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// RepoRoot returns the module root (the directory containing go.mod) so tests
// can reach checked-in artifacts (workflows, gold sets, the plan file)
// regardless of which package directory `go test` runs from.
func RepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}
