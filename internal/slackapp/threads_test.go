// SPDX-License-Identifier: Apache-2.0

package slackapp

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func threadScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	return s
}

// R25: the map is the ConfigMap factory-slack-threads; an entry over 30 days old goes on the next
// Put; a conflict retries three times.
func TestThreadMap(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	c := fake.NewClientBuilder().WithScheme(threadScheme()).Build()
	th := ConfigMapThreads{C: c, Namespace: "agents", Now: func() time.Time { return now }}
	ctx := context.Background()
	if _, ok, err := th.Get(ctx, "aaaaaaaa"); ok || err != nil {
		t.Fatalf("empty map: %v %v", ok, err)
	}
	old := Thread{Channel: "C03", TS: "1.0", At: now.Add(-31 * 24 * time.Hour)}
	if err := th.Put(ctx, "oldroom1", old); err != nil {
		t.Fatal(err)
	}
	fresh := Thread{Channel: "C03", TS: "1729.0001", At: now}
	if err := th.Put(ctx, "aaaaaaaa", fresh); err != nil {
		t.Fatal(err)
	}
	got, ok, err := th.Get(ctx, "aaaaaaaa")
	if err != nil || !ok || got.Channel != "C03" || got.TS != "1729.0001" {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
	if _, ok, _ := th.Get(ctx, "oldroom1"); ok {
		t.Fatal("an entry over 30 days survived the next Put")
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "factory-slack-threads"}, &cm); err != nil {
		t.Fatal(err)
	}
}

func conflictClient(failures int) (client.Client, *int) {
	n := 0
	c := fake.NewClientBuilder().WithScheme(threadScheme()).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			n++
			if n <= failures {
				return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), nil)
			}
			return cl.Update(ctx, obj, opts...)
		},
	}).Build()
	return c, &n
}

func TestThreadMapRetriesConflicts(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	ctx := context.Background()
	for _, tc := range []struct {
		failures int
		wantErr  bool
		attempts int
	}{{2, false, 3}, {3, true, 3}} {
		c, n := conflictClient(tc.failures)
		// the map already exists, so Put takes the Update path
		th := ConfigMapThreads{C: c, Namespace: "agents", Now: func() time.Time { return now }}
		if err := c.Create(ctx, newThreadCM("agents")); err != nil {
			t.Fatal(err)
		}
		err := th.Put(ctx, "aaaaaaaa", Thread{Channel: "C03", TS: "1.0", At: now})
		if (err != nil) != tc.wantErr || *n != tc.attempts {
			t.Fatalf("failures=%d: err=%v attempts=%d", tc.failures, err, *n)
		}
	}
}

// The ConfigMap is bounded: when the encoded map passes MaxBytes the oldest entries go until it
// fits, the newest stays, and the log carries a count, never a room id.
func TestThreadMapStaysBounded(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	c := fake.NewClientBuilder().WithScheme(threadScheme()).Build()
	var logs bytes.Buffer
	th := ConfigMapThreads{C: c, Namespace: "agents", Now: func() time.Time { return now }, MaxBytes: 1500,
		Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	ctx := context.Background()
	for i := range 40 {
		at := now.Add(-time.Duration(40-i) * time.Hour)
		if err := th.Put(ctx, fmt.Sprintf("room%04d", i), Thread{Channel: "C03", TS: "1.0", At: at}); err != nil {
			t.Fatal(err)
		}
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: "agents", Name: "factory-slack-threads"}, &cm); err != nil {
		t.Fatal(err)
	}
	size := 0
	for k, v := range cm.Data {
		size += len(k) + len(v)
	}
	if size > 1500 || len(cm.Data) == 0 || len(cm.Data) >= 40 {
		t.Fatalf("size %d, %d entries", size, len(cm.Data))
	}
	if _, ok := cm.Data["room0039"]; !ok {
		t.Fatal("the newest entry was dropped")
	}
	if _, ok := cm.Data["room0000"]; ok {
		t.Fatal("the oldest entry survived")
	}
	if !strings.Contains(logs.String(), "dropped=") || strings.Contains(logs.String(), "room00") {
		t.Fatalf("log: %s", logs.String())
	}
}
