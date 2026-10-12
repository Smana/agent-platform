// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"testing"
	"time"
)

// #58: readiness ignores GitHub, so each ping publishes whether GitHub answered recently, and the
// value follows it down and up again.
func TestPingerReportsGitHubUp(t *testing.T) {
	ticks := make(chan time.Time)
	up := true
	reported := make(chan bool, 8)
	p := &pinger{ping: func(context.Context) error { return nil }, every: time.Minute,
		up:     func() bool { return up },
		record: func(_ context.Context, v bool) { reported <- v },
		ticker: func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()
	if !<-reported {
		t.Fatal("up after a healthy first ping")
	}
	up = false
	ticks <- time.Time{}
	if <-reported {
		t.Fatal("down once GitHub stopped answering")
	}
	up = true
	ticks <- time.Time{}
	if !<-reported {
		t.Fatal("up again when GitHub returns")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
