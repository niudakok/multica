package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// atomcodeBackend implements Backend by spawning the AtomCode CLI in headless
// non-interactive mode (`atomcode -p <prompt> -y --no-telemetry`) and reading
// the final plain-text answer from stdout.
//
// Why not ACP? atomcode 5.0.3 ships an `acp` subcommand, but its ACP server
// does not implement session/load (agentCapabilities.loadSession=false → all
// resume requests fail with "unhandled request") and the `acp` subcommand
// rejects the global `-c/--continue` flag. Multi-turn chat recovery therefore
// happens through atomcode's OWN local session store, which is keyed by cwd
// (`~/.atomcode/sessions/<hash-of-cwd>/`): the daemon reuses the same workdir
// across turns of a chat (reuse_workdir), so re-invoking `atomcode -c -p` in
// that directory resumes the previous conversation for free.
//
// The backend reports a deterministic SessionID derived from the cwd so the
// daemon's PriorSessionID → ResumeSessionID chain (resume_session=true) keeps
// working across turns; when a ResumeSessionID arrives we inject `-c` to
// continue the local session. `-c` with no prior session silently starts a
// fresh one, so first turns and fresh workdirs are safe.
type atomcodeBackend struct {
	cfg Config
}

// atomcodeBlockedArgs are owned by Multica. AtomCode accepts the task prompt
// as a flag, so custom args must not replace it; -y / --model / --no-telemetry
// are daemon-owned execution controls; -C/--dir would re-point the working
// directory; -c/--continue is daemon-owned (resume is driven by
// opts.ResumeSessionID, never by user-supplied flags).
var atomcodeBlockedArgs = map[string]blockedArgMode{
	"-p":                             blockedWithValue,  // headless prompt
	"--prompt":                       blockedWithValue,  // headless prompt
	"-y":                             blockedStandalone, // auto-approve all tool calls
	"--dangerously-skip-permissions": blockedStandalone, // auto-approve all tool calls
	"-C":                             blockedWithValue,  // working dir is daemon-owned (cmd.Dir)
	"--dir":                          blockedWithValue,  // working dir is daemon-owned (cmd.Dir)
	"-c":                             blockedStandalone, // resume is daemon-owned via opts.ResumeSessionID
	"--continue":                     blockedStandalone, // resume is daemon-owned via opts.ResumeSessionID
	"--model":                        blockedWithValue,  // model is selected by Multica
	"--no-telemetry":                 blockedStandalone, // daemon runs must not phone home
}

// atomcodeSessionID derives a deterministic, cwd-keyed session id so the daemon
// can thread resume_session=true across turns of a multi-turn chat. atomcode's
// local session store is keyed by working directory, so the cwd hash is the
// stable handle: re-invoking with `-c` in the same workdir resumes the same
// conversation.
func atomcodeSessionID(cwd string) string {
	if cwd == "" {
		cwd = "."
	}
	sum := sha256.Sum256([]byte(cwd))
	return "atomcode-" + hex.EncodeToString(sum[:8])
}

func buildAtomcodeArgs(prompt string, opts ExecOptions, logger *slog.Logger) []string {
	args := []string{"-p", prompt, "-y", "--no-telemetry"}
	if opts.ResumeSessionID != "" {
		// Continue atomcode's local session for this workdir. Safe on first
		// turn too: `-c` with no prior session silently starts a new one.
		args = append([]string{"-c"}, args...)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	args = append(args, filterCustomArgs(opts.ExtraArgs, atomcodeBlockedArgs, logger)...)
	args = append(args, filterCustomArgs(opts.CustomArgs, atomcodeBlockedArgs, logger)...)
	return args
}

func (b *atomcodeBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "atomcode"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("atomcode executable not found at %q: %w", execPath, err)
	}
	timeout := opts.Timeout
	runCtx, cancel := runContext(ctx, timeout)
	args := buildAtomcodeArgs(prompt, opts, b.cfg.Logger)

	cmd := exec.CommandContext(runCtx, execPath, args...)
	hideAgentWindow(cmd)
	// args contain the task prompt; never expose it in daemon logs.
	b.cfg.Logger.Info("agent command", "exec", execPath, "provider", "atomcode")
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = buildEnv(b.cfg.Env)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("atomcode stdout pipe: %w", err)
	}
	stderrBuf := newStderrTail(newLogWriter(b.cfg.Logger, "[atomcode:stderr] "), agentStderrTailBytes)
	cmd.Stderr = stderrBuf
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start atomcode: %w", err)
	}
	b.cfg.Logger.Info("atomcode started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model, "resume", opts.ResumeSessionID != "")

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)
	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)

		started := time.Now()
		go func() {
			<-runCtx.Done()
			_ = stdout.Close()
		}()

		var output strings.Builder
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
		for scanner.Scan() {
			line := strings.TrimRight(scanner.Text(), "\r")
			if line == "" {
				continue
			}
			output.WriteString(line)
			output.WriteByte('\n')
			trySend(msgCh, Message{Type: MessageText, Content: line})
		}
		scanErr := scanner.Err()
		if scanErr != nil {
			_ = stdout.Close()
		}
		exitErr := cmd.Wait()
		duration := time.Since(started)

		status := "completed"
		var errMsg string
		if runCtx.Err() == context.DeadlineExceeded {
			status = "timeout"
			errMsg = fmt.Sprintf("atomcode timed out after %s", timeout)
		} else if runCtx.Err() == context.Canceled {
			status = "aborted"
			errMsg = "execution cancelled"
		} else if exitErr != nil {
			status = "failed"
			errMsg = fmt.Sprintf("atomcode exited with error: %v", exitErr)
		}
		if errMsg != "" {
			errMsg = withAgentStderr(errMsg, "atomcode", stderrBuf.Tail())
		}
		b.cfg.Logger.Info("atomcode finished", "pid", cmd.Process.Pid, "status", status, "duration", duration.Round(time.Millisecond).String())
		resCh <- Result{
			Status:     status,
			Output:     strings.TrimSuffix(output.String(), "\n"),
			Error:      errMsg,
			DurationMs: duration.Milliseconds(),
			// Stable cwd-derived id keeps the daemon's resume chain alive:
			// the next turn of this chat carries it back as ResumeSessionID,
			// which buildAtomcodeArgs turns into `-c`.
			SessionID: atomcodeSessionID(opts.Cwd),
		}
	}()
	return &Session{Messages: msgCh, Result: resCh}, nil
}
