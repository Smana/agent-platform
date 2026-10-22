// SPDX-License-Identifier: Apache-2.0

package killswitch

import (
	"context"
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/internal/factory/runs"
)

type store struct {
	runs    map[string]runs.Run
	revoked map[string]string
}

func (s *store) List(context.Context) ([]runs.Run, error) {
	out := []runs.Run{}
	for _, r := range s.runs {
		out = append(out, r)
	}
	return out, nil
}
func (s *store) Annotate(_ context.Context, id string, kv map[string]string) error {
	s.revoked[id] = kv[runs.AnnRevoked]
	return nil
}
func (s *store) Delete(_ context.Context, id string) error { delete(s.runs, id); return nil }

func TestTheStopSweepsEveryRun(t *testing.T) {
	st := &store{revoked: map[string]string{}, runs: map[string]runs.Run{
		"aaaaaaaa": {ID: "aaaaaaaa", Principal: "system:factory", TaskID: "3buqdlot", Phase: "Running"},
		"bbbbbbbb": {ID: "bbbbbbbb", Principal: "human:291", Phase: "Running"}, // requested through the API
		"cccccccc": {ID: "cccccccc", Principal: "human:291", Phase: "Succeeded"},
		// The reconciler's stop revoked and counted it; its claim still drains (F30).
		"dddddddd": {ID: "dddddddd", Principal: "system:factory", TaskID: "3buqdlot", Phase: "Running", Revoked: "manual"},
	}}
	c := fake.NewClientBuilder().Build()
	s := &Sweeper{Reader: c, Namespace: "agent-system", Runs: st}
	if revoked, err := s.Sweep(context.Background()); len(revoked) != 0 || err != nil || len(st.runs) != 4 {
		t.Fatal("no stop object: nothing is touched")
	}
	_ = c.Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMap, Namespace: "agent-system"}})
	revoked, err := s.Sweep(context.Background())
	ids := []string{}
	for _, r := range revoked {
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)
	if err != nil || !slices.Equal(ids, []string{"aaaaaaaa", "bbbbbbbb"}) || st.revoked["bbbbbbbb"] != "manual" || st.revoked["aaaaaaaa"] != "manual" {
		t.Fatalf("every live run, the human's included, each revoked run counted once: %v %v %v", ids, st.revoked, err)
	}
	if _, left := st.runs["dddddddd"]; left {
		t.Fatal("a run already revoked is still deleted, just not counted again")
	}
	if _, left := st.runs["cccccccc"]; !left || len(st.runs) != 1 {
		t.Fatalf("a finished run is left for its record: %v", st.runs)
	}
}

// #55: each run the sweep deletes has its reservation released, so lifting the stop frees its
// room the same day; a failed release fails the sweep, which the next pass retries.
func TestTheSweepReleasesWhatItDeletes(t *testing.T) {
	st := &store{revoked: map[string]string{}, runs: map[string]runs.Run{
		"aaaaaaaa": {ID: "aaaaaaaa", Principal: "human:291", RoomRef: "3kq7x2ma", Phase: "Running"},
		"cccccccc": {ID: "cccccccc", Principal: "human:291", Phase: "Succeeded"},
	}}
	c := fake.NewClientBuilder().WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMap,
		Namespace: "agent-system"}}).Build()
	var released []string
	fail := true
	s := &Sweeper{Reader: c, Namespace: "agent-system", Runs: st, Release: func(_ context.Context, r runs.Run) error {
		if fail {
			fail = false
			return errors.New("ledger busy")
		}
		released = append(released, r.ID)
		return nil
	}}
	if _, err := s.Sweep(context.Background()); err == nil {
		t.Fatal("a failed release is reported")
	}
	st.runs["aaaaaaaa"] = runs.Run{ID: "aaaaaaaa", Principal: "human:291", RoomRef: "3kq7x2ma", Phase: "Running",
		Revoked: "manual"} // its delete is still draining: listed again
	if _, err := s.Sweep(context.Background()); err != nil || !slices.Equal(released, []string{"aaaaaaaa"}) {
		t.Fatalf("released once, the finished run left to the meter: %v %v", released, err)
	}
}
