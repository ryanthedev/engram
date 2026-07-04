// Package importlint is a hermetic import-boundary checker (DW-3.7): it walks
// a source tree and reports any business package under internal/** that
// imports a forbidden transport/framework package. It exists so the
// dependency-direction rule ("no transport imports in business packages")
// gates CI as a plain `go test`, with no external linter required, and — being
// a tree walk keyed on path — automatically covers packages added by later
// phases. The .golangci.yml depguard rule expresses the same boundary for
// editor/IDE use.
package importlint

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Violation is one offending import.
type Violation struct {
	// Package is the internal package path (relative, e.g. "internal/worker").
	Package string
	// File is the offending source file (relative to root).
	File string
	// Import is the forbidden imported package path.
	Import string
}

// Config parameterizes a Check.
type Config struct {
	// ForbiddenPrefixes are import paths business packages may not import
	// (matched as prefixes, so subpackages are covered).
	ForbiddenPrefixes []string
	// AllowedTransportDirs are internal/ subdirectories permitted to import
	// the forbidden packages (the transport edges), matched as path segments
	// like "internal/server".
	AllowedTransportDirs []string
}

// DefaultConfig is Engram's Phase-3 boundary: no gRPC and no generated proto
// API inside business packages; transport lives only at the named edges.
func DefaultConfig() Config {
	return Config{
		ForbiddenPrefixes: []string{
			"google.golang.org/grpc",
			"github.com/ryanthedev/engram/api/engrampb",
		},
		AllowedTransportDirs: []string{
			"internal/server",
			"internal/authgrpc",
			"internal/engramclient",
			"internal/mcp",
		},
	}
}

// Check walks root/internal and returns every forbidden import in a
// non-allowlisted package. Test files (_test.go) are skipped: tests may reach
// across boundaries. The result is deterministic (sorted by file then import).
func Check(root string, cfg Config) ([]Violation, error) {
	internalRoot := filepath.Join(root, "internal")
	var violations []Violation
	err := filepath.WalkDir(internalRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		if allowed(pkgDir, cfg.AllowedTransportDirs) {
			return nil
		}
		imports, err := fileImports(path)
		if err != nil {
			return err
		}
		for _, imp := range imports {
			if forbidden(imp, cfg.ForbiddenPrefixes) {
				violations = append(violations, Violation{
					Package: pkgDir,
					File:    filepath.ToSlash(rel),
					Import:  imp,
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return violations, nil
}

// allowed reports whether pkgDir is (or is under) an allowlisted transport dir.
func allowed(pkgDir string, dirs []string) bool {
	for _, d := range dirs {
		if pkgDir == d || strings.HasPrefix(pkgDir, d+"/") {
			return true
		}
	}
	return false
}

// forbidden reports whether imp matches any forbidden prefix.
func forbidden(imp string, prefixes []string) bool {
	for _, p := range prefixes {
		if imp == p || strings.HasPrefix(imp, p+"/") {
			return true
		}
	}
	return false
}

// fileImports parses the import paths of one Go file.
func fileImports(path string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(f.Imports))
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}
