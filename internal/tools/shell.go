package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	defaultBashTimeout = 120 * time.Second
	maxBashTimeout     = 600 * time.Second
	// cap on bytes retained from a command's output
	maxBashOutput = 64 * 1024
)

type bashTool struct{ cwd string }

type bashArgs struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	Workdir        string `json:"workdir"`
}

func (t *bashTool) def() toolDef {
	return toolDef{
		name: "bash",
		desc: "Run a shell command in the working directory and return its combined stdout/stderr plus exit code. Long-running commands are killed after the timeout (default 120s).",
		params: objSchema(map[string]any{
			"command":        map[string]any{"type": "string", "description": "Shell command to run."},
			"timeoutSeconds": map[string]any{"type": "integer", "description": "Timeout in seconds (default 120, max 600)."},
			"workdir":        map[string]any{"type": "string", "description": "Directory to run in. Defaults to the working directory."},
		}, "command"),
	}
}
func (t *bashTool) Name() string                { d := t.def(); return d.name }
func (t *bashTool) Description() string         { d := t.def(); return d.desc }
func (t *bashTool) Parameters() json.RawMessage { d := t.def(); return d.Parameters() }

// cappedBuffer keeps at most limit bytes, retaining the head and the tail, so a
// noisy command cannot exhaust memory.
type cappedBuffer struct {
	mu       sync.Mutex
	limit    int
	head     []byte
	tail     []byte
	dropping bool
	total    int
}

func newCappedBuffer(limit int) *cappedBuffer {
	return &cappedBuffer{limit: limit}
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	orig := len(p)
	c.total += orig
	half := c.limit / 2
	if len(c.head) < half {
		n := half - len(c.head)
		if n > len(p) {
			n = len(p)
		}
		c.head = append(c.head, p[:n]...)
		p = p[n:]
	}
	if len(p) > 0 {
		c.dropping = true
		c.tail = append(c.tail, p...)
		if len(c.tail) > half {
			trimmed := make([]byte, half)
			copy(trimmed, c.tail[len(c.tail)-half:])
			c.tail = trimmed
		}
	}
	return orig, nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dropping {
		return string(c.head)
	}
	return string(c.head) + fmt.Sprintf("\n... [%d bytes of output omitted] ...\n", c.total-len(c.head)-len(c.tail)) + string(c.tail)
}

func (t *bashTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var a bashArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", fmt.Errorf("command is required")
	}
	workdir := t.cwd
	if strings.TrimSpace(a.Workdir) != "" {
		wd, err := resolve(t.cwd, a.Workdir)
		if err != nil {
			return "", err
		}
		if info, err := os.Stat(wd); err != nil || !info.IsDir() {
			return "", fmt.Errorf("workdir %s is not a directory", a.Workdir)
		}
		workdir = wd
	}

	timeout := defaultBashTimeout
	if a.TimeoutSeconds > 0 {
		timeout = time.Duration(a.TimeoutSeconds) * time.Second
		if timeout > maxBashTimeout {
			timeout = maxBashTimeout
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell := "sh"
	if _, err := exec.LookPath("bash"); err == nil {
		shell = "bash"
	}
	cmd := exec.Command(shell, "-c", a.Command)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "HAL=1")
	buf := newCappedBuffer(maxBashOutput)
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := setProcessGroup(cmd); err != nil {
		return "", err
	}

	start := time.Now()
	runErr := cmd.Start()
	if runErr != nil {
		return "", fmt.Errorf("failed to start command: %w", runErr)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timedOut := false
	select {
	case runErr = <-done:
	case <-runCtx.Done():
		if runCtx.Err() == context.DeadlineExceeded {
			timedOut = true
		}
		killProcessGroup(cmd)
		<-done
		runErr = runCtx.Err()
	}
	elapsed := time.Since(start)

	out := buf.String()
	var status string
	exitCode := 0
	switch {
	case timedOut:
		status = fmt.Sprintf("command timed out after %s and was killed", timeout)
		exitCode = -1
	case runErr != nil:
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
			status = fmt.Sprintf("exit code %d", exitCode)
		} else if ctx.Err() != nil {
			status = "aborted"
			exitCode = -1
		} else {
			return "", runErr
		}
	default:
		status = "exit code 0"
	}

	var b strings.Builder
	if strings.TrimSpace(out) == "" {
		b.WriteString("(no output)\n")
	} else {
		b.WriteString(out)
		if !strings.HasSuffix(out, "\n") {
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "[%s in %s]", status, elapsed.Round(time.Millisecond))
	result := b.String()
	if timedOut || exitCode != 0 {
		return result, fmt.Errorf("%s", status)
	}
	return result, nil
}
