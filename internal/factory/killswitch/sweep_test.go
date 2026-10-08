// SPDX-License-Identifier: Apache-2.0

package killswitch

import (
	"context"
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
	}}
	c := fake.NewClientBuilder().Build()
	s := &Sweeper{Reader: c, Namespace: "agent-system", Runs: st}
	if n, err := s.Sweep(context.Background()); n != 0 || err != nil || len(st.runs) != 3 {
		t.Fatal("no stop object: nothing is touched")
	}
	_ = c.Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMap, Namespace: "agent-system"}})
	n, err := s.Sweep(context.Background())
	if err != nil || n != 2 || st.revoked["bbbbbbbb"] != "manual" || st.revoked["aaaaaaaa"] != "manual" {
		t.Fatalf("every live run, the human's included: %d %v %v", n, st.revoked, err)
	}
	if _, left := st.runs["cccccccc"]; !left || len(st.runs) != 1 {
		t.Fatalf("a finished run is left for its record: %v", st.runs)
	}
}
