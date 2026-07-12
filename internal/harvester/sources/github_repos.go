package sources

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ryanthedev/engram/internal/harvester"
	"github.com/ryanthedev/engram/internal/mcp"
)

var repoRegexp = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var refPathRegexp = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

var allowLocalGitTransport bool

type githubSource struct {
	repos        []repoTarget
	files        []string
	baseURL      string
	maxFileBytes int64
	deps         harvester.Deps
}

type repoTarget struct {
	repo   string
	branch string
	subdir string
}

var _ harvester.Source = (*githubSource)(nil)

func init() {
	harvester.Register("github-repos", func(cfg harvester.SourceConfig, deps harvester.Deps) (harvester.Source, error) {
		reposVal, ok := cfg.Raw["repos"]
		if !ok {
			return nil, fmt.Errorf("harvester: github-repos: missing required config 'repos'")
		}
		repos, err := parseRepoTargets(reposVal)
		if err != nil {
			return nil, fmt.Errorf("harvester: github-repos: invalid 'repos' config: %w", err)
		}
		if len(repos) == 0 {
			return nil, fmt.Errorf("harvester: github-repos: 'repos' list cannot be empty")
		}

		files := []string{"README.md"}
		if fVal, ok := cfg.Raw["files"]; ok {
			parsed, err := parseStringSlice(fVal)
			if err != nil {
				return nil, fmt.Errorf("harvester: github-repos: invalid 'files' config: %w", err)
			}
			if len(parsed) > 0 {
				files = parsed
			}
		}

		baseURL := "https://github.com/"
		if bVal, ok := cfg.Raw["base_url"]; ok {
			if bStr, ok := bVal.(string); ok {
				baseURL = bStr
			} else {
				return nil, fmt.Errorf("harvester: github-repos: 'base_url' must be a string")
			}
		}

		for _, target := range repos {
			tempBaseURL := baseURL
			if !strings.HasSuffix(tempBaseURL, "/") {
				tempBaseURL += "/"
			}
			repoURL := tempBaseURL + target.repo
			if err := validateURL(repoURL); err != nil {
				return nil, fmt.Errorf("harvester: github-repos: invalid transport: %w", err)
			}
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
				return nil, fmt.Errorf("harvester: github-repos: 'max_file_bytes' must be an integer, got %T", mVal)
			}
		}

		return &githubSource{
			repos:        repos,
			files:        files,
			baseURL:      baseURL,
			maxFileBytes: maxFileBytes,
			deps:         deps,
		}, nil
	})
}

// Type returns the source type name.
func (s *githubSource) Type() string {
	return "github-repos"
}

// Mode returns FullHarvest as deleted files are handled by the sweep of Runner.
func (s *githubSource) Mode() harvester.HarvestMode {
	return harvester.FullHarvest
}

// Harvest clones each configured repo and adds glob-matching files to the sink.
func (s *githubSource) Harvest(ctx context.Context, sink harvester.Sink) error {
	for _, target := range s.repos {
		repo := target.repo
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("harvester: github-repos: cancelled: %w", err)
		}

		// 1. Create a fresh temp dir
		tmpDir, err := os.MkdirTemp("", "harvester-github-")
		if err != nil {
			return fmt.Errorf("harvester: github-repos: creating temp dir: %w", err)
		}

		err = func() error {
			defer os.RemoveAll(tmpDir)

			// Construct repo URL: base_url + owner/repo
			baseURL := s.baseURL
			if !strings.HasSuffix(baseURL, "/") {
				baseURL += "/"
			}
			repoURL := baseURL + repo

			if err := validateURL(repoURL); err != nil {
				return fmt.Errorf("invalid transport: %w", err)
			}

			// 2. git clone
			if err := cloneRepo(ctx, target, repoURL, tmpDir); err != nil {
				return err
			}

			// 3. read HEAD SHA
			cmdSHA := exec.CommandContext(ctx, "git", "-C", tmpDir, "rev-parse", "HEAD")
			var stdoutSHA, stderrSHA bytes.Buffer
			cmdSHA.Stdout = &stdoutSHA
			cmdSHA.Stderr = &stderrSHA
			if err := cmdSHA.Run(); err != nil {
				return fmt.Errorf("getting HEAD SHA for %q: %w (stderr: %s)", repo, err, stderrSHA.String())
			}
			headSHA := strings.TrimSpace(stdoutSHA.String())
			sourceVersion := "sha:" + headSHA

			// Keep track of glob match counts
			globMatchCount := make(map[string]int)
			for _, pattern := range s.files {
				globMatchCount[pattern] = 0
			}

			// 4. walk the configured tree
			walkRoot := tmpDir
			if target.subdir != "" {
				walkRoot = filepath.Join(tmpDir, filepath.FromSlash(target.subdir))
			}
			resolvedTmp, err := filepath.EvalSymlinks(tmpDir)
			if err != nil {
				return fmt.Errorf("resolving clone dir for %q: %w", repo, err)
			}
			resolvedWalk, err := filepath.EvalSymlinks(walkRoot)
			if err != nil {
				if target.subdir != "" && os.IsNotExist(err) {
					s.deps.Logger.WarnContext(ctx, "harvester: github-repos: subdir not present",
						slog.String("repo", repo),
						slog.String("subdir", target.subdir),
					)
					return nil
				}
				return fmt.Errorf("resolving walk root for %q: %w", repo, err)
			}
			if resolvedWalk != resolvedTmp && !strings.HasPrefix(resolvedWalk, resolvedTmp+string(os.PathSeparator)) {
				return fmt.Errorf("subdir %q resolves outside cloned repo %q", target.subdir, repo)
			}

			err = filepath.WalkDir(resolvedWalk, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					if d.Name() == ".git" {
						return filepath.SkipDir
					}
					return nil
				}

				if d.Type()&os.ModeSymlink != 0 {
					s.deps.Logger.WarnContext(ctx, "harvester: github-repos: skipping symlink",
						slog.String("repo", repo),
						slog.String("path", path),
					)
					return nil
				}
				if !d.Type().IsRegular() {
					return nil
				}

				// Get relative path to the resolved clone dir.
				relPath, err := filepath.Rel(resolvedTmp, path)
				if err != nil {
					return fmt.Errorf("getting relative path for %s: %w", path, err)
				}

				// Check if relPath matches any glob
				matchedAny := false
				for _, pattern := range s.files {
					matched, err := matchGlob(pattern, relPath)
					if err != nil {
						s.deps.Logger.WarnContext(ctx, "harvester: github-repos: invalid glob pattern",
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

				// Read and check size
				info, err := d.Info()
				if err != nil {
					return fmt.Errorf("getting info for %s: %w", path, err)
				}
				if !info.Mode().IsRegular() {
					return nil
				}
				if info.Size() > s.maxFileBytes {
					s.deps.Logger.WarnContext(ctx, "harvester: github-repos: skipping oversized file",
						slog.String("repo", repo),
						slog.String("path", relPath),
						slog.Int64("size", info.Size()),
						slog.Int64("max_bytes", s.maxFileBytes),
					)
					return nil
				}

				data, err := os.ReadFile(path)
				if err != nil {
					return fmt.Errorf("reading file %s: %w", path, err)
				}

				if int64(len(data)) > s.maxFileBytes {
					s.deps.Logger.WarnContext(ctx, "harvester: github-repos: skipping oversized file after read",
						slog.String("repo", repo),
						slog.String("path", relPath),
					)
					return nil
				}

				// Check for binary
				if bytes.IndexByte(data, 0) != -1 {
					s.deps.Logger.WarnContext(ctx, "harvester: github-repos: skipping binary file",
						slog.String("repo", repo),
						slog.String("path", relPath),
					)
					return nil
				}

				// sink.Add
				doc := mcp.KnowledgeDoc{
					ID:            repo + "/" + relPath,
					Title:         relPath,
					Text:          string(data),
					SourceVersion: sourceVersion,
					Fields: map[string]any{
						"repo": repo,
						"path": relPath,
					},
				}

				if err := sink.Add(doc); err != nil {
					return fmt.Errorf("adding doc to sink: %w", err)
				}

				return nil
			})
			if err != nil {
				return fmt.Errorf("walking files for %q: %w", repo, err)
			}

			// Log warning for any glob matching zero files
			for pattern, count := range globMatchCount {
				if count == 0 {
					s.deps.Logger.WarnContext(ctx, "harvester: github-repos: glob matched zero files",
						slog.String("repo", repo),
						slog.String("pattern", pattern),
					)
				}
			}

			return nil
		}()

		if err != nil {
			return fmt.Errorf("harvester: github-repos: %w", err)
		}
	}

	return nil
}

func parseRepoTargets(val any) ([]repoTarget, error) {
	if val == nil {
		return nil, nil
	}

	var values []any
	switch slice := val.(type) {
	case []string:
		values = make([]any, len(slice))
		for i, repo := range slice {
			values[i] = repo
		}
	case []any:
		values = slice
	default:
		return nil, fmt.Errorf("expected slice, got %T", val)
	}

	targets := make([]repoTarget, 0, len(values))
	for i, value := range values {
		var target repoTarget
		switch item := value.(type) {
		case string:
			target.repo = item
		case map[string]any:
			repoValue, ok := item["repo"]
			if !ok {
				return nil, fmt.Errorf("element at index %d is missing required key 'repo'", i)
			}
			var repoOK bool
			target.repo, repoOK = repoValue.(string)
			if !repoOK {
				return nil, fmt.Errorf("element at index %d key 'repo' must be a string, got %T", i, repoValue)
			}
			if branchValue, ok := item["branch"]; ok {
				var branchOK bool
				target.branch, branchOK = branchValue.(string)
				if !branchOK {
					return nil, fmt.Errorf("element at index %d key 'branch' must be a string, got %T", i, branchValue)
				}
				if err := validateBranch(target.branch); err != nil {
					return nil, fmt.Errorf("invalid branch at index %d: %w", i, err)
				}
			}
			if subdirValue, ok := item["subdir"]; ok {
				var subdirOK bool
				target.subdir, subdirOK = subdirValue.(string)
				if !subdirOK {
					return nil, fmt.Errorf("element at index %d key 'subdir' must be a string, got %T", i, subdirValue)
				}
				cleaned, err := validateSubdir(target.subdir)
				if err != nil {
					return nil, fmt.Errorf("invalid subdir at index %d: %w", i, err)
				}
				target.subdir = cleaned
			}
		default:
			return nil, fmt.Errorf("element at index %d must be a string or map, got %T", i, value)
		}

		if err := validateRepo(target.repo); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func validateRepo(repo string) error {
	if !repoRegexp.MatchString(repo) {
		return fmt.Errorf("invalid repo format %q, must match ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$", repo)
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return fmt.Errorf("invalid repo format %q", repo)
	}
	for _, segment := range parts {
		if segment == "." || segment == ".." {
			return fmt.Errorf("repo %q contains invalid segment %q", repo, segment)
		}
		if strings.HasPrefix(segment, "-") {
			return fmt.Errorf("repo %q contains segment starting with '-' (flag injection guard)", repo)
		}
	}
	return nil
}

func validateBranch(branch string) error {
	if branch == "" {
		return fmt.Errorf("branch cannot be empty")
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("branch %q starts with '-' (flag injection guard)", branch)
	}
	if strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") {
		return fmt.Errorf("branch %q cannot start or end with '/'", branch)
	}
	if !refPathRegexp.MatchString(branch) {
		return fmt.Errorf("branch %q contains invalid characters", branch)
	}
	for _, segment := range strings.Split(branch, "/") {
		if segment == ".." {
			return fmt.Errorf("branch %q contains invalid '..' segment", branch)
		}
	}
	return nil
}

func validateSubdir(subdir string) (string, error) {
	if subdir == "" {
		return "", fmt.Errorf("subdir cannot be empty")
	}
	if filepath.IsAbs(subdir) || strings.HasPrefix(subdir, "/") {
		return "", fmt.Errorf("subdir %q must be repo-relative", subdir)
	}
	if strings.HasPrefix(subdir, "-") {
		return "", fmt.Errorf("subdir %q starts with '-' (flag injection guard)", subdir)
	}
	if !refPathRegexp.MatchString(subdir) {
		return "", fmt.Errorf("subdir %q contains invalid characters", subdir)
	}
	for _, segment := range strings.Split(subdir, "/") {
		if segment == ".." {
			return "", fmt.Errorf("subdir %q contains invalid '..' segment", subdir)
		}
	}
	cleaned := filepath.Clean(filepath.FromSlash(subdir))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("subdir %q escapes the repo root", subdir)
	}
	for _, segment := range strings.Split(filepath.ToSlash(cleaned), "/") {
		if strings.EqualFold(segment, ".git") {
			return "", fmt.Errorf("subdir %q contains reserved .git segment", subdir)
		}
	}
	return filepath.ToSlash(cleaned), nil
}

func cloneRepo(ctx context.Context, target repoTarget, repoURL, tmpDir string) error {
	args := cloneArgs(target, repoURL, tmpDir, target.subdir != "")
	stderr, err := runGitClone(ctx, filepath.Dir(tmpDir), args)
	if err != nil {
		if target.subdir == "" || !sparseUnsupported(stderr) {
			return fmt.Errorf("cloning repo %q: %w (stderr: %s)", target.repo, err, stderr)
		}
		return cloneWithoutSparse(ctx, target, repoURL, tmpDir)
	}
	if target.subdir == "" {
		return nil
	}

	cmd := exec.CommandContext(ctx, "git", "-C", tmpDir, "sparse-checkout", "set", "--", target.subdir)
	var sparseStderr bytes.Buffer
	cmd.Stderr = &sparseStderr
	if err := cmd.Run(); err != nil {
		if sparseUnsupported(sparseStderr.String()) {
			return cloneWithoutSparse(ctx, target, repoURL, tmpDir)
		}
		return fmt.Errorf("setting sparse checkout for repo %q subdir %q: %w (stderr: %s)", target.repo, target.subdir, err, sparseStderr.String())
	}
	return nil
}

func cloneWithoutSparse(ctx context.Context, target repoTarget, repoURL, tmpDir string) error {
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("resetting temp dir for repo %q sparse-checkout fallback: %w", target.repo, err)
	}
	stderr, err := runGitClone(ctx, filepath.Dir(tmpDir), cloneArgs(target, repoURL, tmpDir, false))
	if err != nil {
		return fmt.Errorf("cloning repo %q without sparse checkout: %w (stderr: %s)", target.repo, err, stderr)
	}
	return nil
}

func cloneArgs(target repoTarget, repoURL, tmpDir string, sparse bool) []string {
	args := []string{"clone", "--depth", "1"}
	if sparse {
		args = append(args, "--filter=blob:none", "--sparse")
	}
	if target.branch != "" {
		args = append(args, "--branch", target.branch, "--single-branch")
	}
	return append(args, "--", repoURL, tmpDir)
}

func runGitClone(ctx context.Context, dir string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.String(), err
}

func sparseUnsupported(stderr string) bool {
	message := strings.ToLower(stderr)
	return (strings.Contains(message, "unknown option") &&
		(strings.Contains(message, "sparse") || strings.Contains(message, "filter"))) ||
		strings.Contains(message, "'sparse-checkout' is not a git command") ||
		strings.Contains(message, "unknown subcommand 'sparse-checkout'")
}

func parseStringSlice(val any) ([]string, error) {
	if val == nil {
		return nil, nil
	}
	switch slice := val.(type) {
	case []string:
		return slice, nil
	case []any:
		res := make([]string, len(slice))
		for i, v := range slice {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("element at index %d is not a string (type %T)", i, v)
			}
			res[i] = s
		}
		return res, nil
	default:
		return nil, fmt.Errorf("expected slice, got %T", val)
	}
}

func matchGlob(pattern, relPath string) (bool, error) {
	relPath = filepath.ToSlash(relPath)
	pattern = filepath.ToSlash(pattern)

	idx := strings.Index(pattern, "**/")
	if idx == -1 {
		return path.Match(pattern, relPath)
	}

	prefix := pattern[:idx]
	suffix := pattern[idx+3:]

	if !strings.HasPrefix(relPath, prefix) {
		return false, nil
	}
	rest := relPath[len(prefix):]

	if rest == "" {
		return path.Match(suffix, "")
	}

	curr := rest
	for {
		matched, err := path.Match(suffix, curr)
		if err != nil {
			return false, err
		}
		if matched {
			return true, nil
		}
		slashIdx := strings.Index(curr, "/")
		if slashIdx == -1 {
			break
		}
		curr = curr[slashIdx+1:]
	}
	return false, nil
}

func validateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parsing URL %q: %w", rawURL, err)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme == "https" || scheme == "http" {
		return nil
	}

	if allowLocalGitTransport {
		if scheme == "file" {
			return nil
		}
		if scheme == "" && filepath.IsAbs(rawURL) {
			return nil
		}
	}

	return fmt.Errorf("git transport protocol %q is not allowed for URL %q", u.Scheme, rawURL)
}
