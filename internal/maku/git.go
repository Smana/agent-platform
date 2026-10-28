// SPDX-License-Identifier: Apache-2.0

package maku

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// What one git call may print or take. A branch lists its files with diff and ls-tree, so those two
// may print more; past the cap a call fails closed.
const (
	maxGitOut     = 1 << 20  // stdout of most calls
	maxGitListOut = 16 << 20 // stdout of diff and ls-tree
	maxGitErr     = 64 << 10 // stderr of all
)

// The bounds on one git call: a fetch may need the network for longer, the rest is local or one
// round trip. Variables so a test can shorten them.
var gitTimeout, fetchTimeout = 30 * time.Second, 120 * time.Second

// Git runs one git command in dir and returns its stdout. Tests replace it.
type Git func(ctx context.Context, dir string, args ...string) (string, error)

// capWriter keeps the first max bytes and drops the rest, remembering that it did; it never fails,
// so git is not killed by a short write.
type capWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	if room := w.max - w.buf.Len(); room < n {
		w.truncated = true
		p = p[:max(room, 0)]
	}
	w.buf.Write(p)
	return n, nil
}

// RunGit runs git -c core.hooksPath=/dev/null -C dir args (R15), with an argv and never a shell:
// no hook of the repository runs, a branch's included, no Git LFS smudge, no recursion into
// submodules, and git never prompts. A fetch, an ls-remote and a worktree add are bounded at 2 minutes and any
// other call at 30 seconds. Its stdout is capped (16 MiB for diff and ls-tree, 1 MiB otherwise), its stderr at 64 KiB,
// and an error carries the first line of stderr.
func RunGit(ctx context.Context, dir string, args ...string) (string, error) {
	limit, outMax := gitTimeout, maxGitOut
	switch args[0] {
	case "fetch", "ls-remote":
		limit = fetchTimeout
	case "worktree":
		if len(args) > 1 && args[1] == "add" { // a large checkout takes as long as a fetch
			limit = fetchTimeout
		}
	case "diff", "ls-tree":
		outMax = maxGitListOut
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	argv := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "filter.lfs.smudge=", "-c", "filter.lfs.process=",
		"-c", "filter.lfs.required=false", "-c", "submodule.recurse=false", "-c", "fetch.recurseSubmodules=false", "-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...) //nolint:gosec // git, with an argument list and never a shell; the callers validate the room, the repository and the path
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1")
	out, stderr := &capWriter{max: outMax}, &capWriter{max: maxGitErr}
	cmd.Stdout, cmd.Stderr = out, stderr
	if err := cmd.Run(); err != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return out.buf.String(), fmt.Errorf("git %s timed out after %s", args[0], limit)
		case ctx.Err() != nil:
			return out.buf.String(), fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out.buf.String(), fmt.Errorf("git %s: %s", args[0], OneLine(stderr.buf.String()))
		}
		return out.buf.String(), fmt.Errorf("git %s: %w", args[0], err)
	}
	if out.truncated {
		return out.buf.String(), fmt.Errorf("git %s: the output is too large (more than %d bytes) to check: check the branch out yourself", args[0], outMax)
	}
	return out.buf.String(), nil
}
