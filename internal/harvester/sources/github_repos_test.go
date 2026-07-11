package sources_test

import (
	"bytes"
	"context"
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

	return parentDir, headSHA
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
