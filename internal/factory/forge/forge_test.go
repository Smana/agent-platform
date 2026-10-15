// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"testing"
)

// api is the method set the factory's consumers declare (narrate, intake, reconciler): the
// GitHub adapter and the fake must never drift apart.
type api interface {
	Labeled(ctx context.Context, label string) ([]Item, error)
	LabelEvents(ctx context.Context, number int, label string) ([]LabelEvent, error)
	Issue(ctx context.Context, number int) (Issue, error)
	Comment(ctx context.Context, number int, body string) error
	RecentComments(ctx context.Context, number int) ([]Comment, error)
	AddLabels(ctx context.Context, number int, labels ...string) error
	RemoveLabel(ctx context.Context, number int, label string) error
	PullRequestForBranch(ctx context.Context, branch string) (int, error)
	PullRequest(ctx context.Context, number int) (PR, error)
}

var (
	_ api = (*GitHub)(nil)
)

// Trailer reads only the message's last paragraph, where git interpret-trailers puts trailers, so
// a line in the body or the subject is never read as one. What it returns is a claim.
func TestTrailerReadsOnlyTheTrailerBlock(t *testing.T) {
	for name, c := range map[string]struct{ msg, want string }{
		"the trailer":             {"docs: fix a link\n\nAgent-Run: 7f3cq2xz", "7f3cq2xz"},
		"the last occurrence":     {"docs: fix\n\nAgent-Run: aaaaaaaa\nAgent-Run: 7f3cq2xz\n", "7f3cq2xz"},
		"CRLF":                    {"docs: fix\r\n\r\nAgent-Run: 7f3cq2xz\r\n", "7f3cq2xz"},
		"absent":                  {"docs: fix a link", ""},
		"a body line is not one":  {"docs: fix\n\nAgent-Run: spoofed1\n\nSigned-off-by: a <a@b>", ""},
		"the subject is not one":  {"Agent-Run: spoofed1", ""},
		"another key":             {"docs: fix\n\nAgent-Runs: aaaaaaaa", ""},
		"trailing blank lines":    {"docs: fix\n\nAgent-Run: 7f3cq2xz\n\n\n", "7f3cq2xz"},
		"a key is case-sensitive": {"docs: fix\n\nagent-run: 7f3cq2xz", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := (PR{HeadMessage: c.msg}).Trailer("Agent-Run"); got != c.want {
				t.Errorf("Trailer = %q, want %q", got, c.want)
			}
		})
	}
}

// The review's B1: Trailer is what the head commit claims, never authentication. The harness
// hook marks the model's Agent-* lines and appends the real ones with `git interpret-trailers
// --if-exists add`, but an agent with a shell can skip or rewrite the hook, and anyone can write
// a run's ID. Each case returns what the message says, and callers must fail closed on it.
func TestTrailerIsAClaimNotAProof(t *testing.T) {
	for name, c := range map[string]struct{ msg, want string }{
		// The hook was skipped: the writer's own value is all there is.
		"a forged trailer": {"docs: fix\n\nAgent-Run: forged01", "forged01"},
		// Both lines are in the message; the last is returned and says nothing about the first.
		"a duplicated trailer": {"docs: fix\n\nAgent-Run: 7f3cq2xz\nAgent-Run: forged01", "forged01"},
		// Only a case variant, the hook skipped: the exact key is absent.
		"a case-variant trailer": {"docs: fix\n\nagent-run: forged01", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := (PR{HeadMessage: c.msg}).Trailer("Agent-Run"); got != c.want {
				t.Errorf("Trailer = %q, want %q", got, c.want)
			}
		})
	}
}
