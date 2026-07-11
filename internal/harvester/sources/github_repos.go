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

var allowLocalGitTransport bool

type githubSource struct {
	repos        []string
	files        []string
	baseURL      string
	maxFileBytes int64
	deps         harvester.Deps
}

var _ harvester.Source = (*githubSource)(nil)

func init() {
	harvester.Register("github-repos", func(cfg harvester.SourceConfig, deps harvester.Deps) (harvester.Source, error) {
		reposVal, ok := cfg.Raw["repos"]
		if !ok {
			return nil, fmt.Errorf("harvester: github-repos: missing required config 'repos'")
		}
		repos, err := parseStringSlice(reposVal)
		if err != nil {
			return nil, fmt.Errorf("harvester: github-repos: invalid 'repos' config: %w", err)
		}
		if len(repos) == 0 {
			return nil, fmt.Errorf("harvester: github-repos: 'repos' list cannot be empty")
		}

		// Security validation of owner/repo
		for _, repo := range repos {
			if !repoRegexp.MatchString(repo) {
				return nil, fmt.Errorf("harvester: github-repos: invalid repo format %q, must match ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$", repo)
			}
			parts := strings.Split(repo, "/")
			if len(parts) != 2 {
				return nil, fmt.Errorf("harvester: github-repos: invalid repo format %q", repo)
			}
			for _, segment := range parts {
				if segment == "." || segment == ".." {
					return nil, fmt.Errorf("harvester: github-repos: repo %q contains invalid segment %q", repo, segment)
				}
				if strings.HasPrefix(segment, "-") {
					return nil, fmt.Errorf("harvester: github-repos: repo %q contains segment starting with '-' (flag injection guard)", repo)
				}
			}
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

		for _, repo := range repos {
			tempBaseURL := baseURL
			if !strings.HasSuffix(tempBaseURL, "/") {
				tempBaseURL += "/"
			}
			repoURL := tempBaseURL + repo
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
	for _, repo := range s.repos {
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
			cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--", repoURL, tmpDir)
			cmd.Dir = filepath.Dir(tmpDir)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr

			if err := cmd.Run(); err != nil {
				return fmt.Errorf("cloning repo %q: %w (stderr: %s)", repo, err, stderr.String())
			}

			// 3. read HEAD SHA
			cmdSHA := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
			cmdSHA.Dir = tmpDir
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

			// 4. walk the tree
			err = filepath.WalkDir(tmpDir, func(path string, d os.DirEntry, err error) error {
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

				// Get relative path to tmpDir
				relPath, err := filepath.Rel(tmpDir, path)
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
