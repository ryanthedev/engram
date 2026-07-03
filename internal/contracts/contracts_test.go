// Package contracts_test verifies Phase-0 meta-contracts that live outside
// any one package: the CI pipeline definition (DW-0.1), the doc-comment
// contracts on the five seam interfaces (DW-0.2), the D0 language lock
// (DW-0.6), and the generated proto contract (DW-0.7).
package contracts_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/ryanthedev/engram/api/engrampb"
	"github.com/ryanthedev/engram/internal/testutil"
)

// TestDW_0_1_CIPipelineDefinesBuildTestLintCodegen asserts the checked-in CI
// workflow runs the DW-0.1/0.2/0.7 gates on every push: build, test, lint,
// and the proto-codegen drift check.
func TestDW_0_1_CIPipelineDefinesBuildTestLintCodegen(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("reading ci.yml: %v", err)
	}
	ci := string(b)
	for _, step := range []string{"make build", "make test", "make lint", "make proto-check"} {
		if !strings.Contains(ci, step) {
			t.Errorf("ci.yml missing step %q", step)
		}
	}
	if !strings.Contains(ci, "opensearchproject/opensearch:3.1.0") {
		t.Error("ci.yml integration job must pin opensearch 3.1.0 (D14)")
	}
}

// TestDW_0_2_SeamInterfacesHaveDocCommentContracts parses the five seam
// packages and asserts the named interfaces and every one of their methods
// carry doc comments — the machine-checkable half of "interfaces compile
// with doc-comment contracts" (revive's exported rule enforces the rest of
// the exported surface in `make lint`).
func TestDW_0_2_SeamInterfacesHaveDocCommentContracts(t *testing.T) {
	root := testutil.RepoRoot(t)
	seams := map[string]string{ // interface name → package dir
		"Store":      "internal/store",
		"Retriever":  "internal/retrieval",
		"Extractor":  "internal/ingest",
		"Reconciler": "internal/ingest",
		"Embedder":   "internal/embed",
	}
	for name, dir := range seams {
		iface, doc := findInterface(t, filepath.Join(root, dir), name)
		if iface == nil {
			t.Errorf("interface %s not found in %s", name, dir)
			continue
		}
		if strings.TrimSpace(doc) == "" {
			t.Errorf("interface %s has no doc comment", name)
		}
		for _, m := range iface.Methods.List {
			if len(m.Names) == 0 {
				continue // embedded interface
			}
			if m.Doc.Text() == "" {
				t.Errorf("%s.%s has no doc-comment contract", name, m.Names[0].Name)
			}
		}
	}
}

// findInterface locates a named interface declaration and its doc text in a
// package directory.
func findInterface(t *testing.T, dir, name string) (*ast.InterfaceType, string) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing %s: %v", dir, err)
	}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}
				for _, spec := range gd.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || ts.Name.Name != name {
						continue
					}
					if it, ok := ts.Type.(*ast.InterfaceType); ok {
						doc := gd.Doc.Text()
						if ts.Doc.Text() != "" {
							doc = ts.Doc.Text()
						}
						return it, doc
					}
				}
			}
		}
	}
	return nil, ""
}

// TestDW_0_6_D0ConfirmedInDecisionLog asserts the language decision is
// explicitly locked in the plan's Decision Log.
func TestDW_0_6_D0ConfirmedInDecisionLog(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t),
		".code-foundations", "plans", "2026-06-29-engram-walking-skeleton.md"))
	if err != nil {
		t.Fatalf("reading plan: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "| D0 ") {
			if !strings.Contains(line, "CONFIRMED") || !strings.Contains(line, "language locked") {
				t.Errorf("D0 row exists but does not lock the language: %s", line)
			}
			return
		}
	}
	t.Fatal("plan Decision Log has no D0 row")
}

// TestDW_0_7_GeneratedProtoBuildsAndCarriesEventID proves the generated Go
// code compiles and exposes the locked service contract: Ingest carrying the
// required event_id, and Search. The var _ assignment is a compile-time
// assertion that both RPCs exist with the expected shapes.
func TestDW_0_7_GeneratedProtoBuildsAndCarriesEventID(t *testing.T) {
	var _ interface {
		Ingest(context.Context, *engrampb.IngestRequest, ...grpc.CallOption) (*engrampb.IngestResponse, error)
		Search(context.Context, *engrampb.SearchRequest, ...grpc.CallOption) (*engrampb.SearchResponse, error)
	} = (engrampb.EngramClient)(nil)

	req := &engrampb.IngestRequest{EventId: "ev-1", TenantId: "t1"}
	if req.GetEventId() != "ev-1" {
		t.Errorf("EventId round-trip failed: %q", req.GetEventId())
	}

	// The proto source must document event_id as required (proto3 cannot
	// express it; the contract lives in the schema comment + server check).
	b, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), "api", "proto", "engram.proto"))
	if err != nil {
		t.Fatalf("reading engram.proto: %v", err)
	}
	proto := string(b)
	for _, needle := range []string{"rpc Ingest(", "rpc Search(", "string event_id = 1", "Required"} {
		if !strings.Contains(proto, needle) {
			t.Errorf("engram.proto missing %q", needle)
		}
	}
}
