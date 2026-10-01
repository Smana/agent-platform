// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/github"
	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/metrics"
	"github.com/Smana/agent-platform/internal/verdictpost"
)

// githubAPI is shared with the factory (factory.go); the poster keeps its own timeout.
const (
	posterGitHubTimeout = 20 * time.Second
	posterEvery         = 15 * time.Second
)

// addVerdictPoster adds the leader's verdict poster (SP2 design §3): agents'
// verdicts to their pull request, as SP3's factory App (P28), whose key is
// mounted at dir (ROOMS_GITHUB_APP_DIR). Unset, no poster runs.
func addVerdictPoster(dir string, add func(manager.Runnable) error, roomLog verdictpost.Log, rooms client.Reader,
	ns, publicURL string, m *metrics.Set, log *slog.Logger,
) error {
	if dir == "" {
		return nil
	}
	p := &verdictpost.Poster{Log: roomLog, PublicURL: publicURL, Now: time.Now, Logger: log,
		GitHub:    &github.App{Dir: dir, API: githubAPI, HC: httpx.New(posterGitHubTimeout, nil), Now: time.Now},
		DataClass: roomDataClass(rooms, ns), OnResult: verdictResults(m)}
	return add(posterLoop(p, log, nil))
}

// posterLoop runs p every posterEvery on the leader, and gives p the loop's
// lease flag as Leading (ruling SY). tick paces it; nil means a time.Ticker.
func posterLoop(p *verdictpost.Poster, log *slog.Logger, tick func(time.Duration) (<-chan time.Time, func())) *leaderLoop {
	var leading atomic.Bool
	p.Leading = leading.Load
	return &leaderLoop{every: posterEvery, active: &leading, ticker: tick, run: func(ctx context.Context) {
		if err := p.Once(ctx); err != nil {
			log.Error("verdict poster", "err", err)
		}
	}}
}

// roomDataClass reads a Room's data class; a Room that cannot be read is never
// treated as public.
func roomDataClass(rooms client.Reader, ns string) func(ctx context.Context, room string) string {
	return func(ctx context.Context, room string) string {
		var r v1alpha1.Room
		if err := rooms.Get(ctx, client.ObjectKey{Namespace: ns, Name: room}, &r); err != nil {
			return ""
		}
		return r.Spec.DataClass
	}
}

// verdictResults counts rooms_verdict_posts_total{result}.
func verdictResults(m *metrics.Set) func(ctx context.Context, result string) {
	return func(ctx context.Context, result string) {
		m.VerdictPosts.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
	}
}
