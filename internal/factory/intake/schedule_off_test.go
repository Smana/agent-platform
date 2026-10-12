// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

// #57: with every class off the merger key is optional, and the renovate-red probe, which reads
// checks through it, finds nothing instead of failing the tick.
func TestRenovateProbeSkipsWithoutAMerger(t *testing.T) {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	now := time.Date(2026, 10, 5, 6, 20, 0, 0, time.UTC)
	f := forge.NewFake()
	f.SetOpenPRs(forge.PRSummary{Number: 40, Author: "renovate[bot]", Created: now.Add(-48 * time.Hour)})
	cfg := schedulerCfg()
	cfg.Classes = nil
	sch := scheduler(f, fake.NewClientBuilder().WithScheme(s), func(context.Context) bool { return false }, now, cfg)
	sch.Merger = forge.Absent{}
	if err := sch.Tick(context.Background()); err != nil {
		t.Fatalf("an absent merger must not fail the tick: %v", err)
	}
	var l v1alpha1.TaskList
	_ = sch.Client.List(context.Background(), &l)
	if len(l.Items) != 1 || l.Items[0].Spec.Source.Ref != "link-rot" {
		t.Fatalf("only the probe-less schedule starts a task: %d", len(l.Items))
	}
}
