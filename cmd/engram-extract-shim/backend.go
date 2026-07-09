package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	defaultAgyModel     = "Gemini 3.5 Flash (Low)"
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

// agyBackend is the default backend: `agy -p "<sys+user>" --model <preset>`.
// agy has no system/user split and no JSON-output mode — its plain stdout is
// already clean per the research's live test.
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
	return []string{"-p", combinePrompt(systemPrompt, userPrompt), "--model", model}
}

func (b agyBackend) Run(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	return runProcess(ctx, "agy", b.args(systemPrompt, userPrompt))
}

// codexBackend: `codex exec --skip-git-repo-check --ignore-user-config -o
// FILE -c model_reasoning_effort=low "<sys+user>"`. codex's stdout carries
// banner noise, so output is captured via -o into a shim-controlled temp
// file (never a path derived from event text) and read back after exit.
type codexBackend struct {
	Model string
}

// args builds the argv slice for codex given a shim-controlled outPath (see
// Run) — pure and separate from execution for the same reason as
// agyBackend.args.
func (b codexBackend) args(systemPrompt, userPrompt, outPath string) []string {
	args := []string{"exec", "--skip-git-repo-check", "--ignore-user-config", "-o", outPath, "-c", "model_reasoning_effort=low"}
	if b.Model != "" {
		args = append(args, "-c", "model="+b.Model)
	}
	return append(args, combinePrompt(systemPrompt, userPrompt))
}

func (b codexBackend) Run(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	tmp, err := os.CreateTemp("", "engram-extract-shim-codex-*.txt")
	if err != nil {
		return "", fmt.Errorf("%w: creating codex output file: %v", ErrBackendUnavailable, err)
	}
	outPath := tmp.Name()
	if cerr := tmp.Close(); cerr != nil {
		os.Remove(outPath)
		return "", fmt.Errorf("%w: closing codex output file: %v", ErrBackendUnavailable, cerr)
	}
	defer os.Remove(outPath)

	if _, err := runProcess(ctx, "codex", b.args(systemPrompt, userPrompt, outPath)); err != nil {
		return "", err
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		return "", fmt.Errorf("%w: reading codex output file: %v", ErrBackendUnavailable, err)
	}
	return string(out), nil
}

// claudeBackend: `claude -p "<user>" --system-prompt "<sys>" --model haiku
// --effort low`. claude is the only backend with a true system/user split,
// but its global CLAUDE.md injection (~24k tokens, non-deterministic
// extraction quality per the research) makes it opt-in only — it is never
// selected by a default and must be named explicitly via -backend claude.
type claudeBackend struct {
	Model string
}

// args builds the argv slice for claude, pure and separate from execution
// for the same reason as agyBackend.args. claude is the only backend with a
// true system/user split (--system-prompt), so systemPrompt and userPrompt
// are each their own argv element rather than combined.
func (b claudeBackend) args(systemPrompt, userPrompt string) []string {
	model := b.Model
	if model == "" {
		model = defaultClaudeModel
	}
	return []string{"-p", userPrompt, "--system-prompt", systemPrompt, "--model", model, "--effort", defaultClaudeEffort}
}

func (b claudeBackend) Run(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	return runProcess(ctx, "claude", b.args(systemPrompt, userPrompt))
}
