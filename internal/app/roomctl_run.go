// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/Smana/maku/internal/roomctl"
)

const runUsage = "run --repo owner/name --task TEXT [--role implementer|tester|triager]"

// runRepo is the broker's own repository form (humanapi's repositoryRE).
var runRepo = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

// quickstartRun is `roomctl run`: quickstart-only, so a broker that is not local is refused before
// anything is created; a local one is discovered again, which also proves the forwarded port.
func (r Roomctl) quickstartRun(ctx context.Context, cfg roomctl.Config, c roomctl.Client, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	repo := fs.String("repo", "", "the repository, owner/name: the one your PAT can write")
	task := fs.String("task", "", "what the run should do")
	role := fs.String("role", "implementer", "implementer, tester or triager")
	pos, err := flags(fs, args)
	if err != nil || len(pos) != 0 || !runRepo.MatchString(*repo) || strings.TrimSpace(*task) == "" {
		return fmt.Errorf("%s: %w", runUsage, errors.Join(err, errors.New("--repo owner/name --task TEXT")))
	}
	if cfg.HumanMode != roomctl.HumanModeLocal {
		return errors.New("run is quickstart-only: it needs a local broker (roomctl configure --url http://localhost:<port>); " +
			"on a platform install the factory starts runs")
	}
	got, err := roomctl.Discover(ctx, r.HC, cfg.URL)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	if got.HumanMode != roomctl.HumanModeLocal {
		// The saved config says local, the broker at its URL does not: nothing is created on it.
		return fmt.Errorf("run is quickstart-only: %s does not report a local broker (humanMode %q); on a platform install the factory starts runs",
			cfg.URL, got.HumanMode)
	}
	hints := r.Err
	if hints == nil {
		hints = os.Stderr
	}
	apply := r.Apply
	if apply == nil {
		if _, err := exec.LookPath("kubectl"); err != nil {
			return errors.New("run: kubectl is not on PATH: roomctl run applies the run's claim with your own kubectl and kubeconfig")
		}
		apply = kubectlApply(hints)
	}
	_, err = roomctl.QuickstartRun{Client: c, Repository: *repo, Task: *task, Role: *role, Apply: apply, Out: r.Out}.Do(ctx)
	return err
}

// kubectlApply creates a claim with the developer's kubectl and kubeconfig: the manifest goes on
// stdin, never argv, and kubectl's own output and error reach out as kubectl wrote them.
func kubectlApply(out io.Writer) func(ctx context.Context, manifest []byte) error {
	return func(ctx context.Context, manifest []byte) error {
		cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(manifest), out, out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("kubectl apply -f -: %w (kubectl's message is above)", err)
		}
		return nil
	}
}
