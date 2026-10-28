// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	factoryv1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/api/v1alpha1"
	"github.com/Smana/maku/internal/factory/killswitch"
	"github.com/Smana/maku/internal/factory/runs"
	"github.com/Smana/maku/internal/factory/tracing"
)

func TestRunFactoryRefuses(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	for _, c := range []struct {
		name   string
		getenv func(string) string
		want   string
	}{
		{"without its environment names every missing variable", env(), "FACTORY_CONFIG is not set\nPOD_NAMESPACE is not set"},
		// A bad config fails the rollout (§4): the pod never starts.
		{"with a config it cannot read", env("FACTORY_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"), "POD_NAMESPACE", "agent-system"),
			"no such file or directory"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := RunFactory(ctx, slog.New(slog.DiscardHandler), c.getenv)
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.HasPrefix(err.Error(), "agent-factory: ") {
				t.Fatalf("RunFactory = %v, want an error naming %q", err, c.want)
			}
		})
	}
}

func TestFactoryManagerOptions(t *testing.T) {
	for _, ns := range []string{"agent-system", "factory-test"} {
		t.Run(ns, func(t *testing.T) {
			o := factoryManagerOptions(ns)
			if !o.LeaderElection || o.LeaderElectionID != "agent-factory.agents.ogenki.io" || o.LeaderElectionNamespace != ns ||
				!o.LeaderElectionReleaseOnCancel {
				t.Fatalf("leader election: %+v", o)
			}
			if o.GracefulShutdownTimeout == nil || *o.GracefulShutdownTimeout != managerDrain {
				t.Fatalf("the manager's drain must fit the pod's grace: %v", o.GracefulShutdownTimeout)
			}
			if o.Metrics.BindAddress != "0" || o.HealthProbeBindAddress != "0" {
				t.Fatal(":9090 serves metrics and probes; the manager's own listeners stay off")
			}
			if o.Client.Cache == nil || o.Client.Cache.Unstructured {
				t.Fatal("unstructured reads (a stop's run list) must reach the API server")
			}
			// The chart's Roles grant exactly these namespaces: a cluster-wide informer is refused.
			want := map[string][]string{"Task": {ns}, "Room": {ns}, "ConfigMap": {ns}, runs.GVK().Kind: {runs.Namespace()}}
			if len(o.Cache.ByObject) != len(want) || o.Cache.DefaultNamespaces != nil {
				t.Fatalf("cached kinds: %v, default namespaces %v", o.Cache.ByObject, o.Cache.DefaultNamespaces)
			}
			for obj, by := range o.Cache.ByObject {
				var kind string
				switch x := obj.(type) {
				case *factoryv1.Task:
					kind = "Task"
				case *v1alpha1.Room:
					kind = "Room"
				case *corev1.ConfigMap:
					kind = "ConfigMap"
					// Only the stop object: the factory never lists the namespace's ConfigMaps.
					if by.Field == nil || by.Field.String() != "metadata.name="+killswitch.ConfigMap {
						t.Fatalf("ConfigMaps cached by %v", by.Field)
					}
				case *unstructured.Unstructured:
					kind = x.GroupVersionKind().Kind
					if x.GroupVersionKind() != runs.GVK() {
						t.Fatalf("watches %v, want the AgentRun GVK", x.GroupVersionKind())
					}
				default:
					t.Fatalf("caches %T", obj)
				}
				var got []string
				for n := range by.Namespaces {
					got = append(got, n)
				}
				if !slices.Equal(got, want[kind]) {
					t.Fatalf("%s cached in %v, want %v", kind, got, want[kind])
				}
			}
		})
	}
}

// Every replica pings GitHub at once and then every period, so the token stays warm on a replica
// that is not leading and makes no other call.
func TestPingerPingsAtOnceThenEveryPeriod(t *testing.T) {
	ticks := make(chan time.Time)
	var calls atomic.Int32
	pinged := make(chan struct{}, 8)
	var period time.Duration
	p := &pinger{ping: func(context.Context) error {
		calls.Add(1)
		pinged <- struct{}{}
		return errors.New("a failed ping is only a stale token: the pinger goes on")
	}, every: time.Minute, ticker: func(d time.Duration) (<-chan time.Time, func()) { period = d; return ticks, func() {} }}
	if p.NeedLeaderElection() {
		t.Fatal("every replica pings")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()
	<-pinged
	ticks <- time.Time{}
	<-pinged
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || period != time.Minute {
		t.Fatalf("%d pings, period %s", calls.Load(), period)
	}
}

// R46: no tracing block, no sink, and a nil interface rather than a typed nil, which the
// reconciler would call. A configured endpoint gives an exporter without dialling it: an
// unreachable collector loses spans, never the factory's start.
func TestTaskSink(t *testing.T) {
	sink, tp, shutdown, err := taskSink(t.Context(), "")
	if err != nil || sink != nil || tp != nil {
		t.Fatalf("tracing off: %v %v %v", sink, tp, err)
	}
	shutdown(t.Context())
	sink, tp, shutdown, err = taskSink(t.Context(), "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sink.(*tracing.Exporter); !ok {
		t.Fatalf("tracing on: %T", sink)
	}
	if tp == nil { // F15: the rooms client's provider, or its spans are dead
		t.Fatal("tracing on: no tracer provider for the rooms client")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	shutdown(ctx)
}
