// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// maxGitOut caps what one git call may print, so a hostile repository cannot fill memory.
const maxGitOut = 1 << 20

// Git runs one git command in dir and returns its stdout. Tests replace it.
type Git func(ctx context.Context, dir string, args ...string) (string, error)

// capWriter keeps the first max bytes and drops the rest; it never fails, so git is not killed
// by a short write.
type capWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room < len(p) {
		w.truncated = true
		p = p[:max(room, 0)]
		w.buf.Write(p)
		return len(p), nil
	}
	return w.buf.Write(p)
}

// RunGit runs git -c core.hooksPath=/dev/null -C dir args, with an argv and never a shell (R15):
// no hook of the repository runs, a branch's included, and git never prompts. Its stdout is
// capped at 1 MiB, and an error carries the first line of stderr.
func RunGit(ctx context.Context, dir string, args ...string) (string, error) {
	argv := append([]string{"-c", "core.hooksPath=/dev/null", "-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...) //nolint:gosec // git, with an argument list and never a shell; the callers validate the room, the repository and the path
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out := &capWriter{max: maxGitOut}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out.buf.String(), fmt.Errorf("git %s: %s", args[0], OneLine(stderr.String()))
		}
		return out.buf.String(), fmt.Errorf("git %s: %w", args[0], err)
	}
	if out.truncated {
		return out.buf.String(), fmt.Errorf("git %s: more than %d bytes of output", args[0], maxGitOut)
	}
	return out.buf.String(), nil
}
