package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"log/slog"
)

func TestNewReturnsAtomcodeBackend(t *testing.T) {
	t.Parallel()
	b, err := New("atomcode", Config{ExecutablePath: "/nonexistent/atomcode"})
	if err != nil {
		t.Fatalf("New(atomcode) error: %v", err)
	}
	if _, ok := b.(*atomcodeBackend); !ok {
		t.Fatalf("expected *atomcodeBackend, got %T", b)
	}
}

func TestBuildAtomcodeArgs(t *testing.T) {
	t.Parallel()
	args := buildAtomcodeArgs("task prompt", ExecOptions{
		Model:      "deepseek-v4-flash",
		ExtraArgs:  []string{"--verbose", "--dev"},
		CustomArgs: []string{"--model", "sneaky", "-p", "hijack", "--no-telemetry", "-c", "--lang", "zh-CN"},
	}, slog.Default())

	// Base daemon-owned args come first.
	wantPrefix := []string{"-p", "task prompt", "-y", "--no-telemetry", "--model", "deepseek-v4-flash"}
	if len(args) < len(wantPrefix) {
		t.Fatalf("args too short: %v", args)
	}
	for i, want := range wantPrefix {
		if args[i] != want {
			t.Fatalf("args[%d] = %q, want %q; all=%v", i, args[i], want, args)
		}
	}
	joined := strings.Join(args, " ")
	// User custom args must keep the non-blocked ones and drop blocked ones.
	if !strings.Contains(joined, "--verbose") || !strings.Contains(joined, "--dev") {
		t.Fatalf("non-managed custom args missing from %v", args)
	}
	if strings.Contains(joined, "sneaky") || strings.Contains(joined, "hijack") {
		t.Fatalf("blocked custom args leaked into %v", args)
	}
	// -c must never come from user custom args (resume is daemon-owned).
	if strings.Contains(joined, "-c ") {
		t.Fatalf("user-supplied -c leaked into %v", args)
	}
	// --no-telemetry and -y must appear exactly once (daemon-owned).
	if count := strings.Count(joined, "--no-telemetry"); count != 1 {
		t.Fatalf("--no-telemetry count = %d in %v, want exactly 1", count, args)
	}
	if count := strings.Count(joined, " -y "); count != 1 {
		t.Fatalf("-y count = %d in %v, want exactly 1", count, args)
	}
}

func TestBuildAtomcodeArgsResumeInjectsContinue(t *testing.T) {
	t.Parallel()
	// When the daemon threads a prior session id back (resume_session=true),
	// the backend must inject `-c` so atomcode continues its local session.
	args := buildAtomcodeArgs("follow-up", ExecOptions{
		ResumeSessionID: "atomcode-12345678",
	}, slog.Default())
	if args[0] != "-c" {
		t.Fatalf("expected -c first for resume, got %v", args)
	}
	// A first turn (no ResumeSessionID) must NOT inject -c.
	first := buildAtomcodeArgs("hello", ExecOptions{}, slog.Default())
	if strings.Contains(strings.Join(first, " "), " -c ") {
		t.Fatalf("-c injected without resume request: %v", first)
	}
}

func TestAtomcodeSessionIDStablePerCwd(t *testing.T) {
	t.Parallel()
	a := atomcodeSessionID("/tmp/ws-1")
	b := atomcodeSessionID("/tmp/ws-1")
	c := atomcodeSessionID("/tmp/ws-2")
	if a != b {
		t.Fatalf("session id not stable for same cwd: %q vs %q", a, b)
	}
	if a == c {
		t.Fatalf("session id collides across cwds: %q", a)
	}
	if !strings.HasPrefix(a, "atomcode-") {
		t.Fatalf("session id missing prefix: %q", a)
	}
}

func fakeAtomcodeScript() string {
	return `#!/bin/sh
case "$ATOMCODE_MODE" in
  error)
    echo 'synthetic atomcode failure' >&2
    exit 3
    ;;
  *)
    printf 'line one\nline two\n'
    exit 0
    ;;
esac
`
}

func newFakeAtomcodeBackend(t *testing.T, env map[string]string) *atomcodeBackend {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is POSIX-only")
	}
	path := filepath.Join(t.TempDir(), "atomcode")
	if err := os.WriteFile(path, []byte(fakeAtomcodeScript()), 0o755); err != nil {
		t.Fatalf("write fake atomcode: %v", err)
	}
	return &atomcodeBackend{cfg: Config{ExecutablePath: path, Logger: slog.Default(), Env: env}}
}

func awaitAtomcodeResult(t *testing.T, session *Session) ([]Message, Result) {
	t.Helper()
	var messages []Message
	for message := range session.Messages {
		messages = append(messages, message)
	}
	select {
	case result, ok := <-session.Result:
		if !ok {
			t.Fatal("result channel closed without a result")
		}
		return messages, result
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for atomcode result")
		return nil, Result{}
	}
}

func TestAtomcodeBackendCollectsStdout(t *testing.T) {
	t.Parallel()
	b := newFakeAtomcodeBackend(t, nil)
	session, err := b.Execute(context.Background(), "hello", ExecOptions{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	messages, result := awaitAtomcodeResult(t, session)

	var text []string
	for _, msg := range messages {
		if msg.Type == MessageText {
			text = append(text, msg.Content)
		}
	}
	if strings.Join(text, "\n") != "line one\nline two" {
		t.Fatalf("streamed text = %q, want %q", strings.Join(text, "\n"), "line one\nline two")
	}
	if result.Status != "completed" {
		t.Fatalf("status = %q, want completed", result.Status)
	}
	if result.Output != "line one\nline two" {
		t.Fatalf("output = %q, want %q", result.Output, "line one\nline two")
	}
	if !strings.HasPrefix(result.SessionID, "atomcode-") {
		t.Fatalf("session id = %q, want atomcode- prefix", result.SessionID)
	}
}

func TestAtomcodeBackendExitError(t *testing.T) {
	t.Parallel()
	b := newFakeAtomcodeBackend(t, map[string]string{"ATOMCODE_MODE": "error"})
	session, err := b.Execute(context.Background(), "hello", ExecOptions{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_, result := awaitAtomcodeResult(t, session)
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if !strings.Contains(result.Error, "exit status 3") && !strings.Contains(result.Error, "synthetic atomcode failure") {
		t.Fatalf("error = %q, want exit detail or stderr tail", result.Error)
	}
}
