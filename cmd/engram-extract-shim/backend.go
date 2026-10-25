package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// killWaitDelay bounds how long cmd.Run waits, after the context is
// cancelled and the process is signalled, before force-closing the process's
// I/O pipes and returning. Without it a grandchild that inherited the stdout
// pipe keeps that pipe open, so Run's output-copy goroutine blocks and Run
// hangs until every descendant exits (10-30s observed with agy/codex/claude,
// which routinely fork helper subprocesses). See DW-1.3.
const killWaitDelay = 2 * time.Second

// ErrBackendUnavailable reports that the selected CLI backend could not
// produce output — a non-zero exit, a context timeout, or an I/O failure
// getting at its output. The HTTP layer maps this to a retryable status
// (never a hang, never a silent 500 that would dead-letter the event).
var ErrBackendUnavailable = errors.New("engram-extract-shim: backend unavailable")

// ErrUnknownBackend reports an unrecognized -backend/SHIM_BACKEND value.
var ErrUnknownBackend = errors.New("engram-extract-shim: unknown backend")

// Default cheap-model, low-effort configuration per backend (research:
// .code-foundations/research/2026-07-08-extraction-cli-shim.md).
const (
	defaultAgyModel     = "Gemini 3.8 Flash (Low)" // agy retired the 3.5 preset; check `agy models` when bumping
	defaultClaudeModel  = "haiku"
	defaultClaudeEffort = "low"
)

// Backend is the Strategy interface (GoF): invoke a headless agent CLI with
// a system and user prompt and return its raw stdout. Concrete backends
// differ only in how they assemble argv and where they read output from —
// exactly the "family of interchangeable algorithms" Strategy is for; a
// switch-on-backend-name inside the HTTP handler was rejected because it
// would make the test-only fake backend impossible without a production
// code branch just for tests.
type Backend interface {
	Run(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// newBackend constructs the named Backend. model, when empty, falls back to
// the backend's built-in cheap-model default. Unknown names fail fast at
// startup (a bad -backend flag is a configuration bug, not a runtime
// condition to recover from).
func newBackend(name, model string) (Backend, error) {
	switch name {
	case "", "agy":
		return agyBackend{Model: model}, nil
	case "codex":
		return codexBackend{Model: model}, nil
	case "claude":
		return claudeBackend{Model: model}, nil
	case "ensemble":
		// The judge is always claude-sonnet-5 (judgeModel, ensemble.go) — an
		// internal constant per the plan, never the -model override, which
		// only threads through to the two candidate extractors.
		return ensembleBackend{
			Agy:   agyBackend{Model: model},
			Codex: codexBackend{Model: model},
			Judge: claudeBackend{Model: judgeModel},
		}, nil
	default:
		return nil, fmt.Errorf("%w: %q (want agy, codex, claude, or ensemble)", ErrUnknownBackend, name)
	}
}

// combinePrompt joins system and user prompts for backends with no native
// system/user split (agy, codex per the research's tested CLI behavior).
func combinePrompt(systemPrompt, userPrompt string) string {
	if systemPrompt == "" {
		return userPrompt
	}
	return systemPrompt + "\n\n" + userPrompt
}

// runProcess executes name with args as an argv slice — never through a
// shell, never via string concatenation — so event-derived text in args can
// contain any byte sequence (shell metacharacters, newlines, NUL-adjacent
// garbage) without ever being interpreted as shell syntax (DW-1.4). Process
// failure and context-deadline expiry both map to ErrBackendUnavailable so
// callers get one uniform, retryable failure mode instead of a hang.
//
// Two mechanisms guarantee a hung or forking backend cannot stall the caller
// past the context deadline (DW-1.3's "timeout -> not a hang"):
//   - The process runs in its own process group (Setpgid), and on
//     cancellation the whole group is SIGKILLed — reaping grandchildren that
//     agy/codex/claude spawn, not just the direct child.
//   - cmd.WaitDelay backstops that: even if a descendant survives the group
//     kill, Run force-closes the inherited I/O pipes and returns within
//     killWaitDelay rather than blocking on the output-copy goroutine.
func runProcess(ctx context.Context, name string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid targets the whole process group, killing descendants
		// that inherited the stdout pipe (the actual cause of the hang);
		// fall back to the direct process if the group signal fails.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = killWaitDelay
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrBackendUnavailable, name, ctx.Err())
	}
	if runErr != nil {
		return "", fmt.Errorf("%w: %s exited: %v: %s", ErrBackendUnavailable, name, runErr, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Every backend runs its CLI as close to a plain completion endpoint as the
// CLI allows: the event text is untrusted (pasted content, tool output, web
// harvests), so tools, MCP servers, user hooks and saved sessions are turned
// off wherever a flag exists, and each backend enforces factsSchema through
// its CLI's structured-output flag. Run returns the schema-shaped JSON for
// parseFacts.
//
// Checked 2026-10-25 by asking each CLI to read a canary file outside its
// working directory: claude and codex (with the flags below) could not; codex
// with only -s read-only could. agy has no flag to turn its tools off; it
// could not read the canary either, and its working directory is the
// launchd agent's empty ~/.engram/shim.

// agyBackend is the default backend: `agy -p "<sys+user>" --model <preset>
// --sandbox --disable-slash-commands --json-schema <schema> --output-format
// json`. agy has no system/user split, so the prompts are combined.
type agyBackend struct {
	Model string
}

// args builds the argv slice for agy, kept as a pure function separate from
// Run so DW-1.4 (event text passed inert) can be verified structurally —
// asserting the dirty text lands as one opaque slice element — without
// shelling out to the real agy binary.
func (b agyBackend) args(systemPrompt, userPrompt string) []string {
	model := b.Model
	if model == "" {
		model = defaultAgyModel
	}
	return []string{"-p", combinePrompt(systemPrompt, userPrompt), "--model", model,
		"--sandbox", "--disable-slash-commands", "--json-schema", factsSchema, "--output-format", "json"}
}

func (b agyBackend) Run(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	out, err := runProcess(ctx, "agy", b.args(systemPrompt, userPrompt))
	if err != nil {
		return "", err
	}
	return structuredOutput("agy", out)
}

// codexBackend: `codex exec <codexNoToolArgs> --skip-git-repo-check
// --ignore-user-config --ephemeral -s read-only --output-schema FILE -o FILE
// -c model_reasoning_effort=low "<sys+user>"`. codex's stdout carries banner
// noise, so the final message is captured via -o. Both files live in a
// shim-controlled temp dir (never a path derived from event text).
type codexBackend struct {
	Model string
}

// codexNoToolArgs disable codex's tools. -s read-only alone still lets codex
// run commands that read any file the user can (it read the canary).
var codexNoToolArgs = []string{
	"--disable", "shell_tool", "--disable", "unified_exec", "--disable", "code_mode_host",
	"--disable", "view_image", "--disable", "sleep_tool", "--disable", "browser_use",
	"--disable", "computer_use", "--disable", "apps", "-c", "web_search=disabled",
}

// args builds the argv slice for codex given shim-controlled schema and
// output paths (see Run) — pure and separate from execution for the same
// reason as agyBackend.args.
func (b codexBackend) args(systemPrompt, userPrompt, schemaPath, outPath string) []string {
	args := append([]string{"exec"}, codexNoToolArgs...)
	args = append(args, "--skip-git-repo-check", "--ignore-user-config", "--ephemeral", "-s", "read-only",
		"--output-schema", schemaPath, "-o", outPath, "-c", "model_reasoning_effort=low")
	if b.Model != "" {
		args = append(args, "-c", "model="+b.Model)
	}
	return append(args, combinePrompt(systemPrompt, userPrompt))
}

func (b codexBackend) Run(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	dir, err := os.MkdirTemp("", "engram-extract-shim-codex-*")
	if err != nil {
		return "", fmt.Errorf("%w: creating codex temp dir: %v", ErrBackendUnavailable, err)
	}
	defer os.RemoveAll(dir)
	schemaPath := filepath.Join(dir, "schema.json")
	outPath := filepath.Join(dir, "out.json")
	if err := os.WriteFile(schemaPath, []byte(factsSchema), 0o600); err != nil {
		return "", fmt.Errorf("%w: writing codex schema file: %v", ErrBackendUnavailable, err)
	}

	if _, err := runProcess(ctx, "codex", b.args(systemPrompt, userPrompt, schemaPath, outPath)); err != nil {
		return "", err
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		return "", fmt.Errorf("%w: reading codex output file: %v", ErrBackendUnavailable, err)
	}
	return string(out), nil
}

// claudeBackend: `claude -p "<user>" --system-prompt "<sys>" --model haiku
// --effort low` plus the isolation flags below. claude is the only backend
// with a true system/user split. Without isolation every call loaded ~30.7k
// tokens of the user's CLAUDE.md, tools and plugins; with it, ~525 (measured
// 2026-10-25). --bare would do the same but reads only ANTHROPIC_API_KEY,
// and this shim runs on the user's subscription login.
type claudeBackend struct {
	Model string
}

// claudeIsolationArgs turn claude into a completion endpoint: no tools, no
// MCP servers, no user/project/local settings (so no hooks, plugins or
// CLAUDE.md), and no session file per call.
var claudeIsolationArgs = []string{"--tools", "", "--strict-mcp-config", "--setting-sources", "", "--no-session-persistence"}

// args builds the argv slice for claude, pure and separate from execution
// for the same reason as agyBackend.args. claude is the only backend with a
// true system/user split (--system-prompt), so systemPrompt and userPrompt
// are each their own argv element rather than combined.
func (b claudeBackend) args(systemPrompt, userPrompt string) []string {
	model := b.Model
	if model == "" {
		model = defaultClaudeModel
	}
	args := []string{"-p", userPrompt, "--system-prompt", systemPrompt, "--model", model, "--effort", defaultClaudeEffort}
	args = append(args, claudeIsolationArgs...)
	return append(args, "--json-schema", factsSchema, "--output-format", "json")
}

func (b claudeBackend) Run(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	out, err := runProcess(ctx, "claude", b.args(systemPrompt, userPrompt))
	if err != nil {
		return "", err
	}
	return structuredOutput("claude", out)
}

// structuredOutput unwraps the --output-format json envelope that claude and
// agy print: the schema-validated structured_output when present, else the
// plain response text for parseFacts to try. An envelope reporting an error,
// or one with neither field, is a backend failure (retryable), never "[]".
func structuredOutput(name, stdout string) (string, error) {
	var env struct {
		StructuredOutput json.RawMessage `json:"structured_output"`
		Result           string          `json:"result"`   // claude
		Response         string          `json:"response"` // agy
		IsError          bool            `json:"is_error"` // claude
		Status           string          `json:"status"`   // agy
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		return "", fmt.Errorf("%w: %s: decoding json envelope: %v", ErrBackendUnavailable, name, err)
	}
	if env.IsError || (env.Status != "" && env.Status != "SUCCESS") {
		return "", fmt.Errorf("%w: %s reported an error: %.300s", ErrBackendUnavailable, name, stdout)
	}
	if len(env.StructuredOutput) > 0 && string(env.StructuredOutput) != "null" {
		return string(env.StructuredOutput), nil
	}
	if text := env.Result + env.Response; strings.TrimSpace(text) != "" {
		return text, nil
	}
	return "", fmt.Errorf("%w: %s: envelope carried no output", ErrBackendUnavailable, name)
}
