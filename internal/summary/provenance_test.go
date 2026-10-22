// SPDX-License-Identifier: Apache-2.0

package summary_test

import (
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/summary"
)

// R10: the room's summary shows the current run's provenance as the factory posted it.
func TestTheSummaryShowsTheRunsProvenance(t *testing.T) {
	factory := envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}
	facts := envelope.TaskFacts{Phase: "Implementing", Run: &envelope.RunFact{ID: "cf4ato2x", Role: "implementer",
		Provenance: &envelope.ProvenanceFact{FactoryVersion: "v0.16.0", CompositionRevision: "xagentruns.cloud.ogenki.io-41b6efe"}}}
	s := summary.View(summary.Fold([]envelope.Event{ev(1, envelope.StateChanged, factory, envelope.TaskStatePayload(facts))}),
		"r", "u", owner(), 0, time.Now())
	if s.Status.Run == nil || s.Status.Run.Provenance == nil || s.Status.Run.Provenance.FactoryVersion != "v0.16.0" ||
		s.Status.Run.Provenance.CompositionRevision != "xagentruns.cloud.ogenki.io-41b6efe" {
		t.Fatalf("%+v", s.Status.Run)
	}
}
