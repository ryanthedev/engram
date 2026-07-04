package importlint

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ryanthedev/engram/internal/testutil"
)

// TestDW_3_7_GreenOnCleanTree: the real internal/ tree has no transport import
// outside the allowlisted edges — the boundary holds today, and this test
// fails CI the moment any later-phase package imports gRPC or the proto API.
func TestDW_3_7_GreenOnCleanTree(t *testing.T) {
	root := testutil.RepoRoot(t)
	violations, err := Check(root, DefaultConfig())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(violations) != 0 {
		for _, v := range violations {
			t.Errorf("boundary violation: %s imports %s (%s)", v.Package, v.Import, v.File)
		}
	}
}

// TestDW_3_7_RedOnTransportImport: a fixture package under internal/ that
// imports gRPC is flagged — proving the rule actually bites (the red case CI
// would fail on).
func TestDW_3_7_RedOnTransportImport(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "internal", "badpkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := `package badpkg

import (
	"context"

	_ "google.golang.org/grpc"
)

func Use(context.Context) {}
`
	if err := os.WriteFile(filepath.Join(pkgDir, "bad.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	violations, err := Check(root, DefaultConfig())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %d, want 1", len(violations))
	}
	if violations[0].Package != "internal/badpkg" || violations[0].Import != "google.golang.org/grpc" {
		t.Fatalf("violation = %+v, want internal/badpkg importing grpc", violations[0])
	}
}

// TestDW_3_7_AllowlistedEdgeIgnored: the same forbidden import inside an
// allowlisted transport dir is NOT flagged (transport is permitted there).
func TestDW_3_7_AllowlistedEdgeIgnored(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "internal", "server")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := "package server\n\nimport _ \"google.golang.org/grpc\"\n"
	if err := os.WriteFile(filepath.Join(pkgDir, "s.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	violations, err := Check(root, DefaultConfig())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none (server is an allowlisted edge)", violations)
	}
}
