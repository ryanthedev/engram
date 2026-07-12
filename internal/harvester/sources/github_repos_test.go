package sources_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryanthedev/engram/internal/harvester"
	"github.com/ryanthedev/engram/internal/harvester/sources"
	"github.com/ryanthedev/engram/internal/mcp"
	"google.golang.org/protobuf/types/known/structpb"
)

type testSink struct {
	docs []mcp.KnowledgeDoc
}

func (s *testSink) Add(doc mcp.KnowledgeDoc) error {
	s.docs = append(s.docs, doc)
	return nil
}

func (s *testSink) Flush(ctx context.Context) error {
	return nil
}

func createLocalRepo(t *testing.T, owner, repo string, files map[string]string) (string, string) {
	t.Helper()
	parentDir := t.TempDir()
	headSHA := createLocalRepoAt(t, parentDir, owner, repo, files)
	return parentDir, headSHA
}

func createLocalRepoAt(t *testing.T, parentDir, owner, repo string, files map[string]string) string {
	t.Helper()
	repoDir := filepath.Join(parentDir, owner, repo)
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("failed to create repo dir: %v", err)
	}

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v failed: %v (stderr: %s)", args, err, stderr.String())
		}
		return strings.TrimSpace(stdout.String())
	}

	runGit("init")
	runGit("config", "user.name", "Test User")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "core.autocrlf", "false")

	for relPath, content := range files {
		absPath := filepath.Join(repoDir, relPath)
		if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
			t.Fatalf("failed to create dir for file %s: %v", relPath, err)
		}
		if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write file %s: %v", relPath, err)
		}
		runGit("add", relPath)
	}

	runGit("commit", "-m", "initial commit")
	headSHA := runGit("rev-parse", "HEAD")

	return headSHA
}

func runGitInRepo(t *testing.T, repoDir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v failed: %v (stderr: %s)", args, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

func TestGithubReposBranch(t *testing.T) {
	owner := "testowner"
	repo := "branchrepo"
	parentDir, mainSHA := createLocalRepo(t, owner, repo, map[string]string{
		"README.md": "main branch",
	})
	repoDir := filepath.Join(parentDir, owner, repo)
	runGitInRepo(t, repoDir, "checkout", "-b", "docs")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("docs branch"), 0644); err != nil {
		t.Fatalf("failed to update README on docs branch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "DOCS.md"), []byte("branch-only file"), 0644); err != nil {
		t.Fatalf("failed to write branch-only file: %v", err)
	}
	runGitInRepo(t, repoDir, "add", "README.md", "DOCS.md")
	runGitInRepo(t, repoDir, "commit", "-m", "docs branch content")
	docsSHA := runGitInRepo(t, repoDir, "rev-parse", "HEAD")

	cfg := harvester.SourceConfig{Type: "github-repos", Raw: map[string]any{
		"repos": []any{map[string]any{"repo": owner + "/" + repo, "branch": "docs"}},
		"files": []string{"*.md"}, "base_url": parentDir,
	}}
	src, err := harvester.Build(cfg, harvester.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}
	sink := &testSink{}
	if err := src.Harvest(context.Background(), sink); err != nil {
		t.Fatalf("Harvest failed: %v", err)
	}
	if len(sink.docs) != 2 {
		t.Fatalf("expected 2 docs from docs branch, got %d", len(sink.docs))
	}
	for _, doc := range sink.docs {
		if doc.SourceVersion != "sha:"+docsSHA {
			t.Errorf("expected docs branch SHA %q, got %q", docsSHA, doc.SourceVersion)
		}
		if doc.SourceVersion == "sha:"+mainSHA {
			t.Errorf("source version unexpectedly used default branch SHA %q", mainSHA)
		}
	}
	if sink.docs[0].Text != "branch-only file" && sink.docs[1].Text != "branch-only file" {
		t.Error("branch-only file was not harvested")
	}
}

func TestGithubReposSubdir(t *testing.T) {
	owner := "testowner"
	repo := "subdirrepo"
	parentDir, _ := createLocalRepo(t, owner, repo, map[string]string{
		"docs/reference.md": "reference docs",
		"docs/guide.md":     "guide docs",
		"other/private.md":  "outside docs",
	})
	cfg := harvester.SourceConfig{Type: "github-repos", Raw: map[string]any{
		"repos": []any{map[string]any{"repo": owner + "/" + repo, "subdir": "docs"}},
		"files": []string{"docs/**/*.md"}, "base_url": parentDir,
	}}
	src, err := harvester.Build(cfg, harvester.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}
	sink := &testSink{}
	if err := src.Harvest(context.Background(), sink); err != nil {
		t.Fatalf("Harvest failed: %v", err)
	}
	want := map[string]bool{
		owner + "/" + repo + "/docs/reference.md": true,
		owner + "/" + repo + "/docs/guide.md":     true,
	}
	if len(sink.docs) != len(want) {
		t.Fatalf("expected %d docs, got %d", len(want), len(sink.docs))
	}
	for _, doc := range sink.docs {
		if !want[doc.ID] {
			t.Errorf("unexpected doc ID %q", doc.ID)
		}
	}
}

func TestGithubReposSubdirSymlinkCannotEscapeClone(t *testing.T) {
	owner := "testowner"
	repo := "symlinkescape"
	parentDir, _ := createLocalRepo(t, owner, repo, map[string]string{
		"README.md": "inside repo",
	})
	repoDir := filepath.Join(parentDir, owner, repo)
	outsideDir := t.TempDir()
	outsideSubdir := filepath.Join(outsideDir, "sub")
	if err := os.Mkdir(outsideSubdir, 0755); err != nil {
		t.Fatalf("failed to create outside subdir: %v", err)
	}
	const outsideContent = "must not be harvested"
	if err := os.WriteFile(filepath.Join(outsideSubdir, "outside.md"), []byte(outsideContent), 0644); err != nil {
		t.Fatalf("failed to write outside file: %v", err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(repoDir, "link")); err != nil {
		t.Fatalf("failed to create escaping symlink: %v", err)
	}
	runGitInRepo(t, repoDir, "add", "link")
	runGitInRepo(t, repoDir, "commit", "-m", "add escaping symlink")

	cfg := harvester.SourceConfig{Type: "github-repos", Raw: map[string]any{
		"repos":    []any{map[string]any{"repo": owner + "/" + repo, "subdir": "link/sub"}},
		"files":    []string{"**/*.md"},
		"base_url": parentDir,
	}}
	src, err := harvester.Build(cfg, harvester.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}
	sink := &testSink{}
	err = src.Harvest(context.Background(), sink)
	if err == nil {
		t.Fatal("expected escaping subdir symlink to fail harvest")
	}
	if !strings.Contains(err.Error(), "subdir") || !strings.Contains(err.Error(), "outside") {
		t.Errorf("expected clear subdir escape error, got %v", err)
	}
	for _, doc := range sink.docs {
		if doc.Text == outsideContent {
			t.Fatalf("harvest ingested file outside clone: %#v", doc)
		}
	}
}

func TestGithubReposRejectsGitMetadataSubdir(t *testing.T) {
	for _, subdir := range []string{".git", ".git/config", "docs/.GiT/config"} {
		t.Run(subdir, func(t *testing.T) {
			cfg := harvester.SourceConfig{Type: "github-repos", Raw: map[string]any{
				"repos":    []any{map[string]any{"repo": "owner/repo", "subdir": subdir}},
				"base_url": filepath.Join(t.TempDir(), "must-not-be-cloned"),
			}}
			_, err := harvester.Build(cfg, harvester.Deps{Logger: slog.Default()})
			if err == nil {
				t.Fatalf("expected subdir %q to be rejected", subdir)
			}
			if !strings.Contains(strings.ToLower(err.Error()), ".git") {
				t.Errorf("expected .git validation error, got %v", err)
			}
		})
	}
}

func TestGithubReposRejectsInvalidBranchAndSubdir(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
	}{
		{name: "branch flag", field: "branch", value: "-x"},
		{name: "branch shell text", field: "branch", value: "main;rm -rf /"},
		{name: "branch parent segment", field: "branch", value: ".."},
		{name: "branch space", field: "branch", value: "a b"},
		{name: "absolute subdir", field: "subdir", value: "/etc"},
		{name: "parent subdir", field: "subdir", value: "../secrets"},
		{name: "subdir flag", field: "subdir", value: "-flag"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "git-side-effect")
			target := map[string]any{"repo": "owner/repo", tc.field: tc.value}
			cfg := harvester.SourceConfig{Type: "github-repos", Raw: map[string]any{
				"repos":    []any{target},
				"base_url": filepath.Dir(marker),
			}}
			_, err := harvester.Build(cfg, harvester.Deps{Logger: slog.Default()})
			if err == nil {
				t.Fatalf("expected %s %q to be rejected", tc.field, tc.value)
			}
			if !strings.Contains(err.Error(), "invalid "+tc.field) {
				t.Errorf("expected invalid %s error, got %v", tc.field, err)
			}
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Errorf("validation produced side effect at %q", marker)
			}
		})
	}
}

func TestGithubReposMixedStringAndMapTargets(t *testing.T) {
	parentDir := t.TempDir()
	createLocalRepoAt(t, parentDir, "owner", "plain", map[string]string{"README.md": "plain"})
	createLocalRepoAt(t, parentDir, "owner", "mapped", map[string]string{"README.md": "mapped"})
	manifest, err := harvester.LoadManifest([]byte(fmt.Sprintf(`
collections:
  - name: docs
    sources:
      - type: github-repos
        repos:
          - owner/plain
          - { repo: owner/mapped }
        base_url: %q
`, parentDir)))
	if err != nil {
		t.Fatalf("failed to parse mixed target manifest: %v", err)
	}
	cfg := manifest.Collections[0].Sources[0]
	src, err := harvester.Build(cfg, harvester.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("failed to build mixed targets: %v", err)
	}
	sink := &testSink{}
	if err := src.Harvest(context.Background(), sink); err != nil {
		t.Fatalf("Harvest failed: %v", err)
	}
	if len(sink.docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(sink.docs))
	}
	want := map[string]bool{"owner/plain/README.md": true, "owner/mapped/README.md": true}
	for _, doc := range sink.docs {
		if !want[doc.ID] {
			t.Errorf("unexpected doc ID %q", doc.ID)
		}
	}
}

func TestDW_4_3_SecurityValidation(t *testing.T) {
	badRepos := []string{
		"foo/bar; rm -rf x",
		"../etc",
		"-oProxyCommand=evil",
		"a/b$(touch pwned)",
		"./foo",
		"foo/../bar",
		"-foo/bar",
		"foo/-bar",
		"foo/.",
		"foo/..",
		"owner/repo/sub",
		"owner",
	}

	for _, repo := range badRepos {
		t.Run(repo, func(t *testing.T) {
			cfg := harvester.SourceConfig{
				Type: "github-repos",
				Raw: map[string]any{
					"repos": []string{repo},
				},
			}
			_, err := harvester.Build(cfg, harvester.Deps{Logger: slog.Default()})
			if err == nil {
				t.Errorf("expected repo %q to be rejected, but it succeeded", repo)
			} else {
				if !strings.Contains(err.Error(), "invalid repo") &&
					!strings.Contains(err.Error(), "contains invalid segment") &&
					!strings.Contains(err.Error(), "segment starting with '-'") {
					t.Errorf("unexpected error message for %q: %v", repo, err)
				}
			}
		})
	}
}

func TestDW_4_1_HappyPath(t *testing.T) {
	files := map[string]string{
		"README.md":   "This is the README",
		"docs/a.md":   "This is docs/a.md",
		"other/b.txt": "This should be matched if pattern matches",
		"nested/c.md": "This is nested/c.md",
	}
	owner := "testowner"
	repo := "testrepo"
	parentDir, headSHA := createLocalRepo(t, owner, repo, files)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := harvester.Deps{Logger: logger}

	cfg := harvester.SourceConfig{
		Type: "github-repos",
		Raw: map[string]any{
			"repos":    []string{owner + "/" + repo},
			"files":    []string{"README.md", "docs/**/*.md", "**/*.txt"},
			"base_url": parentDir,
		},
	}

	src, err := harvester.Build(cfg, deps)
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}

	if src.Type() != "github-repos" {
		t.Errorf("expected Type() 'github-repos', got %q", src.Type())
	}
	if src.Mode() != harvester.FullHarvest {
		t.Errorf("expected Mode() FullHarvest, got %v", src.Mode())
	}

	sink := &testSink{}
	ctx := context.Background()

	if err := src.Harvest(ctx, sink); err != nil {
		t.Fatalf("Harvest failed: %v", err)
	}

	// README.md, docs/a.md, other/b.txt match patterns.
	// nested/c.md does not match:
	// README.md matches "README.md"
	// docs/a.md matches "docs/**/*.md"
	// other/b.txt matches "**/*.txt"
	// nested/c.md does not match README.md, docs/**/*.md, or **/*.txt
	expectedDocs := map[string]string{
		"testowner/testrepo/README.md":   "This is the README",
		"testowner/testrepo/docs/a.md":   "This is docs/a.md",
		"testowner/testrepo/other/b.txt": "This should be matched if pattern matches",
	}

	if len(sink.docs) != len(expectedDocs) {
		t.Fatalf("expected %d docs, got %d", len(expectedDocs), len(sink.docs))
	}

	for _, doc := range sink.docs {
		expectedText, ok := expectedDocs[doc.ID]
		if !ok {
			t.Errorf("unexpected doc ID: %q", doc.ID)
			continue
		}
		if doc.Text != expectedText {
			t.Errorf("expected Text %q, got %q", expectedText, doc.Text)
		}
		expectedRelPath := strings.TrimPrefix(doc.ID, owner+"/"+repo+"/")
		if doc.Title != expectedRelPath {
			t.Errorf("expected Title %q, got %q", expectedRelPath, doc.Title)
		}
		if doc.SourceVersion != "sha:"+headSHA {
			t.Errorf("expected SourceVersion %q, got %q", "sha:"+headSHA, doc.SourceVersion)
		}
		if doc.Fields["repo"] != owner+"/"+repo {
			t.Errorf("expected Fields.repo %q, got %v", owner+"/"+repo, doc.Fields["repo"])
		}
		if doc.Fields["path"] != expectedRelPath {
			t.Errorf("expected Fields.path %q, got %v", expectedRelPath, doc.Fields["path"])
		}
	}
}

func TestDW_4_2_DeletionsShrinkEmittedSet(t *testing.T) {
	files := map[string]string{
		"README.md": "This is the README",
		"docs/a.md": "This is docs/a.md",
	}
	owner := "testowner"
	repo := "testrepo"
	parentDir, headSHA1 := createLocalRepo(t, owner, repo, files)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deps := harvester.Deps{Logger: logger}

	cfg := harvester.SourceConfig{
		Type: "github-repos",
		Raw: map[string]any{
			"repos":    []string{owner + "/" + repo},
			"files":    []string{"README.md", "docs/**/*.md"},
			"base_url": parentDir,
		},
	}

	src, err := harvester.Build(cfg, deps)
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}

	sink1 := &testSink{}
	if err := src.Harvest(context.Background(), sink1); err != nil {
		t.Fatalf("First Harvest failed: %v", err)
	}

	if len(sink1.docs) != 2 {
		t.Fatalf("expected 2 docs in first harvest, got %d", len(sink1.docs))
	}

	// Delete docs/a.md in local repo
	repoDir := filepath.Join(parentDir, owner, repo)
	if err := os.Remove(filepath.Join(repoDir, "docs/a.md")); err != nil {
		t.Fatalf("failed to delete file: %v", err)
	}

	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v failed: %v (stderr: %s)", args, err, stderr.String())
		}
		return strings.TrimSpace(stdout.String())
	}
	runGit("add", "docs/a.md")
	runGit("commit", "-m", "delete docs/a.md")
	headSHA2 := runGit("rev-parse", "HEAD")

	if headSHA1 == headSHA2 {
		t.Fatalf("expected head SHA to change after commit")
	}

	sink2 := &testSink{}
	if err := src.Harvest(context.Background(), sink2); err != nil {
		t.Fatalf("Second Harvest failed: %v", err)
	}

	if len(sink2.docs) != 1 {
		t.Fatalf("expected 1 doc in second harvest, got %d", len(sink2.docs))
	}

	doc := sink2.docs[0]
	if doc.ID != owner+"/"+repo+"/README.md" {
		t.Errorf("expected remaining doc to be README.md, got %q", doc.ID)
	}
	if doc.SourceVersion != "sha:"+headSHA2 {
		t.Errorf("expected source version to be new SHA %q, got %q", "sha:"+headSHA2, doc.SourceVersion)
	}
}

func TestGithubReposEdges(t *testing.T) {
	t.Run("glob matching zero files warning", func(t *testing.T) {
		files := map[string]string{
			"README.md": "README content",
		}
		owner := "testowner"
		repo := "testrepo"
		parentDir, _ := createLocalRepo(t, owner, repo, files)

		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, nil))
		deps := harvester.Deps{Logger: logger}

		cfg := harvester.SourceConfig{
			Type: "github-repos",
			Raw: map[string]any{
				"repos":    []string{owner + "/" + repo},
				"files":    []string{"nonexistent*.txt"},
				"base_url": parentDir,
			},
		}

		src, err := harvester.Build(cfg, deps)
		if err != nil {
			t.Fatalf("failed to build: %v", err)
		}

		sink := &testSink{}
		if err := src.Harvest(context.Background(), sink); err != nil {
			t.Fatalf("Harvest failed: %v", err)
		}

		if len(sink.docs) != 0 {
			t.Errorf("expected 0 docs, got %d", len(sink.docs))
		}

		if !strings.Contains(logBuf.String(), "glob matched zero files") {
			t.Errorf("expected warning log about zero matches, got: %s", logBuf.String())
		}
	})

	t.Run("unreachable clone target error", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		deps := harvester.Deps{Logger: logger}

		cfg := harvester.SourceConfig{
			Type: "github-repos",
			Raw: map[string]any{
				"repos":    []string{"nonexistent/repo"},
				"base_url": "/path/does/not/exist",
			},
		}

		src, err := harvester.Build(cfg, deps)
		if err != nil {
			t.Fatalf("failed to build: %v", err)
		}

		sink := &testSink{}
		err = src.Harvest(context.Background(), sink)
		if err == nil {
			t.Fatal("expected Harvest to fail due to unreachable target, but it succeeded")
		}
		if !strings.Contains(err.Error(), "cloning repo") {
			t.Errorf("expected clone error, got: %v", err)
		}
	})

	t.Run("binary and oversized files skipped", func(t *testing.T) {
		files := map[string]string{
			"README.md":     "Normal text README",
			"binary.txt":    "binary\x00file",
			"oversized.txt": "this file is quite long",
		}
		owner := "testowner"
		repo := "testrepo"
		parentDir, _ := createLocalRepo(t, owner, repo, files)

		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, nil))
		deps := harvester.Deps{Logger: logger}

		cfg := harvester.SourceConfig{
			Type: "github-repos",
			Raw: map[string]any{
				"repos":          []string{owner + "/" + repo},
				"files":          []string{"README.md", "binary.txt", "oversized.txt"},
				"base_url":       parentDir,
				"max_file_bytes": 20,
			},
		}

		// Let's refine sizes:
		// "Normal text README" is 18 bytes. Since max_file_bytes = 20, it is kept.
		// "this file is quite long" is 23 bytes. Since max_file_bytes = 20, it is skipped.
		// "binary\x00file" is 11 bytes. Since it contains NUL, it is skipped.

		src, err := harvester.Build(cfg, deps)
		if err != nil {
			t.Fatalf("failed to build: %v", err)
		}

		sink := &testSink{}
		if err := src.Harvest(context.Background(), sink); err != nil {
			t.Fatalf("Harvest failed: %v", err)
		}

		if len(sink.docs) != 1 {
			t.Errorf("expected 1 doc (README.md), got %d", len(sink.docs))
		} else {
			if sink.docs[0].ID != owner+"/"+repo+"/README.md" {
				t.Errorf("expected README.md, got %q", sink.docs[0].ID)
			}
		}

		logStr := logBuf.String()
		if !strings.Contains(logStr, "skipping oversized file") {
			t.Errorf("expected log about skipping oversized file, got: %s", logStr)
		}
		if !strings.Contains(logStr, "skipping binary file") {
			t.Errorf("expected log about skipping binary file, got: %s", logStr)
		}
	})
}

func TestMain(m *testing.M) {
	cleanup := sources.ExportedSetAllowLocalGitTransport(true)
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func TestGithubReposSymlinkSkip(t *testing.T) {
	files := map[string]string{
		"normal.txt": "normal file content",
	}
	owner := "testowner"
	repo := "testrepo"
	parentDir, _ := createLocalRepo(t, owner, repo, files)
	repoDir := filepath.Join(parentDir, owner, repo)

	// Create a symlink pointing to normal.txt
	symlinkPath := filepath.Join(repoDir, "symlink.txt")
	if err := os.Symlink("normal.txt", symlinkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	// Commit the symlink
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v failed: %v (stderr: %s)", args, err, stderr.String())
		}
	}
	runGit("add", "symlink.txt")
	runGit("commit", "-m", "commit symlink")

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	deps := harvester.Deps{Logger: logger}

	cfg := harvester.SourceConfig{
		Type: "github-repos",
		Raw: map[string]any{
			"repos":    []string{owner + "/" + repo},
			"files":    []string{"**/*.txt"},
			"base_url": parentDir,
		},
	}

	src, err := harvester.Build(cfg, deps)
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}

	sink := &testSink{}
	if err := src.Harvest(context.Background(), sink); err != nil {
		t.Fatalf("Harvest failed: %v", err)
	}

	// Verify that symlink.txt was skipped
	foundSymlink := false
	foundNormal := false
	for _, doc := range sink.docs {
		if strings.HasSuffix(doc.ID, "symlink.txt") {
			foundSymlink = true
		}
		if strings.HasSuffix(doc.ID, "normal.txt") {
			foundNormal = true
		}
	}

	if !foundNormal {
		t.Errorf("expected normal.txt to be ingested")
	}
	if foundSymlink {
		t.Errorf("expected symlink.txt to be skipped, but it was ingested")
	}

	// Verify log contains skipping symlink message
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "skipping symlink") {
		t.Errorf("expected log to mention skipping symlink, got: %s", logOutput)
	}
}

func TestGithubReposTransportAllowlist(t *testing.T) {
	// Ensure allowLocalGitTransport is false (the default production value)
	cleanup := sources.ExportedSetAllowLocalGitTransport(false)
	defer cleanup()

	tests := []struct {
		name        string
		baseURL     string
		expectError bool
	}{
		{
			name:        "valid https",
			baseURL:     "https://github.com/",
			expectError: false,
		},
		{
			name:        "valid http",
			baseURL:     "http://github.com/",
			expectError: false,
		},
		{
			name:        "invalid file scheme",
			baseURL:     "file:///etc",
			expectError: true,
		},
		{
			name:        "invalid local absolute path",
			baseURL:     "/usr/bin",
			expectError: true,
		},
		{
			name:        "invalid ext scheme",
			baseURL:     "ext::sh -c evil",
			expectError: true,
		},
		{
			name:        "invalid ssh scheme",
			baseURL:     "ssh://git@github.com",
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := harvester.SourceConfig{
				Type: "github-repos",
				Raw: map[string]any{
					"repos":    []string{"owner/repo"},
					"base_url": tc.baseURL,
				},
			}
			_, err := harvester.Build(cfg, harvester.Deps{Logger: slog.Default()})
			if tc.expectError {
				if err == nil {
					t.Errorf("expected build to fail for base_url %q, but it succeeded", tc.baseURL)
				} else if !strings.Contains(err.Error(), "invalid transport") && !strings.Contains(err.Error(), "git transport protocol") {
					t.Errorf("unexpected error for base_url %q: %v", tc.baseURL, err)
				}
			} else {
				if err != nil {
					t.Errorf("expected build to succeed for base_url %q, got error: %v", tc.baseURL, err)
				}
			}
		})
	}
}

// buildScopedGithub builds the github-repos source and asserts it is a
// ScopedSource (per-repo mark-and-sweep scopes).
func buildScopedGithub(t *testing.T, cfg harvester.SourceConfig) harvester.ScopedSource {
	t.Helper()
	src, err := harvester.Build(cfg, harvester.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("failed to build source: %v", err)
	}
	scoped, ok := src.(harvester.ScopedSource)
	if !ok {
		t.Fatalf("github-repos source does not implement harvester.ScopedSource")
	}
	return scoped
}

// TestGithubReposSweepScopesPerRepo asserts each configured repo becomes its own
// per-repo sweep scope (github-repos:owner/repo), deduplicated — the core of the
// fix that stops one repo's run from sweeping another repo's docs.
func TestGithubReposSweepScopesPerRepo(t *testing.T) {
	parentDir, _ := createLocalRepo(t, "orgA", "repoA", map[string]string{"README.md": "a"})
	createLocalRepoAt(t, parentDir, "orgB", "repoB", map[string]string{"README.md": "b"})

	scoped := buildScopedGithub(t, harvester.SourceConfig{Type: "github-repos", Raw: map[string]any{
		// Duplicate orgA/repoA to prove scopes are deduplicated.
		"repos":    []any{"orgA/repoA", "orgB/repoB", "orgA/repoA"},
		"base_url": parentDir,
	}})

	got := scoped.SweepScopes()
	want := []string{"github-repos:orgA/repoA", "github-repos:orgB/repoB"}
	if len(got) != len(want) {
		t.Fatalf("expected %d deduplicated scopes, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("scope %d: expected %q, got %q", i, w, got[i])
		}
	}
}

// TestGithubReposHarvestScopeIsolation proves HarvestScope for one repo emits
// ONLY that repo's docs — so the Runner ingests+sweeps each repo under its own
// source string and repo B's run can never touch repo A's docs.
func TestGithubReposHarvestScopeIsolation(t *testing.T) {
	parentDir, _ := createLocalRepo(t, "orgA", "repoA", map[string]string{"README.md": "alpha"})
	createLocalRepoAt(t, parentDir, "orgB", "repoB", map[string]string{"README.md": "bravo"})

	scoped := buildScopedGithub(t, harvester.SourceConfig{Type: "github-repos", Raw: map[string]any{
		"repos":    []any{"orgA/repoA", "orgB/repoB"},
		"base_url": parentDir,
	}})

	sink := &testSink{}
	if err := scoped.HarvestScope(context.Background(), "github-repos:orgA/repoA", sink); err != nil {
		t.Fatalf("HarvestScope failed: %v", err)
	}
	if len(sink.docs) != 1 {
		t.Fatalf("expected exactly 1 doc from repoA scope, got %d", len(sink.docs))
	}
	if got := sink.docs[0].ID; got != "orgA/repoA/README.md" {
		t.Errorf("expected repoA doc, got %q", got)
	}
	if repo, _ := sink.docs[0].Fields["repo"].(string); repo != "orgA/repoA" {
		t.Errorf("expected repo field orgA/repoA, got %q", repo)
	}
}

// TestGithubReposDocFieldsStructpbEncodable is the structpb-encodability
// regression guard: KnowledgeDoc.Fields is wire-encoded via structpb.NewStruct
// in engramclient.KnowledgeIngest, which REJECTS typed slices (e.g. []string).
// A fake Sink never exercises that encoder, so this asserts the real doc a
// github-repos harvest emits round-trips through structpb.NewStruct.
func TestGithubReposDocFieldsStructpbEncodable(t *testing.T) {
	parentDir, _ := createLocalRepo(t, "orgA", "repoA", map[string]string{"README.md": "alpha"})
	scoped := buildScopedGithub(t, harvester.SourceConfig{Type: "github-repos", Raw: map[string]any{
		"repos":    []any{"orgA/repoA"},
		"base_url": parentDir,
	}})

	sink := &testSink{}
	if err := scoped.HarvestScope(context.Background(), "github-repos:orgA/repoA", sink); err != nil {
		t.Fatalf("HarvestScope failed: %v", err)
	}
	if len(sink.docs) == 0 {
		t.Fatal("expected at least one harvested doc")
	}
	for _, doc := range sink.docs {
		if _, err := structpb.NewStruct(doc.Fields); err != nil {
			t.Fatalf("doc %q Fields are not structpb-encodable (breaks KnowledgeIngest): %v", doc.ID, err)
		}
	}
}
