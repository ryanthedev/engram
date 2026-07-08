package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dirtyEventText carries a spread of shell metacharacters and an embedded
// newline — the exact hazard class DW-1.4 guards against: event text is
// external input that must never reach a shell's parser.
const dirtyEventText = "rm -rf /; $(whoami) && echo pwned || true | cat\nsecond line with `backticks` and \"quotes\" and $VAR"

// TestDW_1_4_ShellMetacharactersPassedInert proves — structurally, per
// backend — that dirty event text lands as exactly one opaque argv element,
// byte-for-byte unchanged. If any backend built its command via a shell
// string instead of an arg-slice, this text would either be split across
// multiple arguments (on the embedded newline/spaces) or trigger command
// substitution; neither can happen when the value is a single slice element
// passed straight to exec.Command.
func TestDW_1_4_ShellMetacharactersPassedInert(t *testing.T) {
	// agy and codex combine system+user into one prompt argument (per the
	// research's tested CLI behavior), so the dirty text is a contiguous
	// substring of that one element rather than the whole element.
	t.Run("agy", func(t *testing.T) {
		args := agyBackend{}.args("sys", dirtyEventText)
		assertOpaqueSubstringInOneArg(t, args, dirtyEventText)
	})
	t.Run("codex", func(t *testing.T) {
		args := codexBackend{}.args("sys", dirtyEventText, "/tmp/out.txt")
		assertOpaqueSubstringInOneArg(t, args, dirtyEventText)
	})
	// claude has a true system/user split (--system-prompt), so each prompt
	// is its own whole argv element.
	t.Run("claude user prompt", func(t *testing.T) {
		args := claudeBackend{}.args("sys", dirtyEventText)
		assertExactSingleArg(t, args, dirtyEventText)
	})
	t.Run("claude system prompt", func(t *testing.T) {
		args := claudeBackend{}.args(dirtyEventText, "user")
		assertExactSingleArg(t, args, dirtyEventText)
	})
}

// assertExactSingleArg fails unless want appears as exactly one whole
// element of args, unmodified — never split, never merged with a
// neighboring flag.
func assertExactSingleArg(t *testing.T, args []string, want string) {
	t.Helper()
	count := 0
	for _, a := range args {
		if a == want {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("dirty text appeared as %d exact argv elements (want exactly 1) in %#v", count, args)
	}
}

// assertOpaqueSubstringInOneArg fails unless want appears, byte-for-byte and
// unsplit, inside exactly one element of args (the combined system+user
// prompt) and nowhere else — proving the dirty text rode along as an opaque
// blob rather than being parsed or fragmented.
func assertOpaqueSubstringInOneArg(t *testing.T, args []string, want string) {
	t.Helper()
	count := 0
	for _, a := range args {
		if strings.Contains(a, want) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("dirty text appeared as a substring of %d argv elements (want exactly 1) in %#v", count, args)
	}
}

// TestDW_1_4_RealSubprocessNeverShellInterprets is the behavioral half of
// DW-1.4: run the dirty text through the real runProcess exec path with
// /bin/echo standing in for a CLI backend. A shell given this text would
// split it on ";"/"&&"/"|", expand $(whoami)/$VAR, and echo would never see
// it as a single argument — /bin/echo's stdout would differ from the input.
// exec.CommandContext with an arg-slice guarantees byte-identical echo.
func TestDW_1_4_RealSubprocessNeverShellInterprets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := runProcess(ctx, "/bin/echo", []string{"-n", dirtyEventText})
	if err != nil {
		t.Fatalf("runProcess(/bin/echo) err = %v", err)
	}
	if out != dirtyEventText {
		t.Fatalf("echo roundtrip = %q, want exactly %q (a shell would have interpreted the metacharacters)", out, dirtyEventText)
	}
}

// TestDW_1_3_BackendProcessNonZeroExit covers the process-level half of
// DW-1.3: a real subprocess that exits non-zero maps to ErrBackendUnavailable.
func TestDW_1_3_BackendProcessNonZeroExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := runProcess(ctx, "/usr/bin/false", nil)
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("runProcess(/usr/bin/false) err = %v, want wrapping ErrBackendUnavailable", err)
	}
}

// TestDW_1_3_BackendProcessTimeout covers the process-level half of DW-1.3:
// a backend that outlives its context deadline is killed and reported as
// ErrBackendUnavailable, never left to hang.
func TestDW_1_3_BackendProcessTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := runProcess(ctx, "/bin/sleep", []string{"5"})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("runProcess(sleep 5, 50ms timeout) err = %v, want wrapping ErrBackendUnavailable", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("runProcess took %v to return after a 50ms timeout — it hung instead of killing the process", elapsed)
	}
}

// TestDW_1_3_ForkingBackendDoesNotHang is the regression test for the
// reviewed blocker: a backend that backgrounds long-lived children which
// inherit the stdout pipe must NOT keep runProcess blocked past the context
// deadline. `sh` forks two 30s sleeps (one backgrounded, one foreground);
// both inherit sh's stdout pipe. Before the fix, SIGKILL hit only `sh` and
// Run() blocked on the output-copy goroutine until the sleeps exited (~30s).
// With the process-group kill + WaitDelay, Run() must return promptly —
// asserted well under the 30s child lifetime — with a retryable timeout
// error. Hermetic (uses /bin/sh, no real CLI) so it stays in the default
// `go test ./...` run.
func TestDW_1_3_ForkingBackendDoesNotHang(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := runProcess(ctx, "/bin/sh", []string{"-c", "sleep 30 & sleep 30"})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("forking backend under a 100ms deadline: err = %v, want wrapping ErrBackendUnavailable", err)
	}
	// killWaitDelay (2s) is the worst-case backstop; add generous CI slack but
	// stay far below the 30s child lifetime so a genuine hang still fails.
	if bound := killWaitDelay + 4*time.Second; elapsed > bound {
		t.Fatalf("runProcess took %v to return after a 100ms deadline (bound %v) — a forking backend still hangs it", elapsed, bound)
	}
}

// TestCodexBackend_RunReadsOutputFile exercises codexBackend end to end
// against a fake "codex" script on PATH that writes to whatever -o path it's
// given — proving the full args-build -> exec -> read-temp-file wiring
// works without requiring the real codex CLI/auth in CI.
func TestCodexBackend_RunReadsOutputFile(t *testing.T) {
	dir := t.TempDir()
	installFakeCodex(t, dir, `#!/bin/sh
outfile=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then
    outfile="$arg"
  fi
  prev="$arg"
done
printf '%s' '[{"subject":"rtd","predicate":"prefers","object":"tabs"}]' > "$outfile"
`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := (codexBackend{}).Run(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("Run() err = %v", err)
	}
	if !strings.Contains(out, `"subject":"rtd"`) {
		t.Fatalf("Run() output = %q, want it to contain the fake codex's written fact", out)
	}
}

// TestCodexBackend_RunPropagatesNonZeroExit confirms codexBackend surfaces a
// retryable error (rather than silently reading a stale/empty temp file)
// when the codex process itself fails.
func TestCodexBackend_RunPropagatesNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	installFakeCodex(t, dir, "#!/bin/sh\nexit 1\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := (codexBackend{}).Run(context.Background(), "sys", "user")
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("Run() err = %v, want wrapping ErrBackendUnavailable", err)
	}
}

func installFakeCodex(t *testing.T, dir, script string) {
	t.Helper()
	path := filepath.Join(dir, "codex")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake codex script: %v", err)
	}
}

func TestNewBackend(t *testing.T) {
	tests := []struct {
		name        string
		backendName string
		wantType    Backend
	}{
		{name: "empty defaults to agy", backendName: "", wantType: agyBackend{}},
		{name: "explicit agy", backendName: "agy", wantType: agyBackend{}},
		{name: "codex", backendName: "codex", wantType: codexBackend{}},
		{name: "claude", backendName: "claude", wantType: claudeBackend{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := newBackend(tc.backendName, "")
			if err != nil {
				t.Fatalf("newBackend(%q) err = %v", tc.backendName, err)
			}
			if got, want := backendKind(b), backendKind(tc.wantType); got != want {
				t.Fatalf("newBackend(%q) = %T, want %T", tc.backendName, b, tc.wantType)
			}
		})
	}
}

func TestNewBackend_UnknownNameIsAnError(t *testing.T) {
	_, err := newBackend("ollama", "")
	if !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("newBackend(\"ollama\") err = %v, want wrapping ErrUnknownBackend", err)
	}
}

// TestNewBackend_ClaudeNeverSelectedByDefault pins the plan's "opt-in only"
// requirement: no empty/unset -backend value ever resolves to claude.
func TestNewBackend_ClaudeNeverSelectedByDefault(t *testing.T) {
	b, err := newBackend("", "")
	if err != nil {
		t.Fatalf("newBackend(\"\") err = %v", err)
	}
	if backendKind(b) == backendKind(claudeBackend{}) {
		t.Fatal("newBackend(\"\") selected claude — must default to agy, claude is opt-in only")
	}
}

func backendKind(b Backend) string {
	switch b.(type) {
	case agyBackend:
		return "agy"
	case codexBackend:
		return "codex"
	case claudeBackend:
		return "claude"
	default:
		return "unknown"
	}
}
