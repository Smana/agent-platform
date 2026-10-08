// SPDX-License-Identifier: Apache-2.0

package triage

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

// classForge is the part of the forge triage reads: an issue's labels, and who applied them.
// (The forge package exports no interface; each consumer declares its own.)
type classForge interface {
	Issue(ctx context.Context, number int) (forge.Issue, error)
	LabelEvents(ctx context.Context, number int, label string) ([]forge.LabelEvent, error)
}

// Matrix is §2's table: the predicted class and the tier choose the team.
func Matrix(class, source, tier string) string {
	switch {
	case source == "runlore":
		return "investigate"
	case class == "docs-links" && tier == "frontier":
		return "pair"
	case class == "docs-links":
		return "solo"
	case tier == "frontier":
		return "trio"
	}
	return "pair"
}

// Control puts percent % of tasks in the OD-14 control group, deterministically by task id.
func Control(taskID string, percent int) bool {
	h := fnv.New32a()
	_, _ = h.Write([]byte(taskID))
	return int64(h.Sum32()%100) < int64(percent) // widened, never narrowed (gosec G115)
}

// Classify is the one triage per task (§2): deterministic rules plus one C7 call.
type Classify struct {
	Cfg   *config.Config
	C     Classifier
	Forge classForge
}

// Triage fixes the class, then asks C7 for the tier: one call per task, whose failure falls
// back to static/standard rather than blocking the task (§2, C7).
func (c Classify) Triage(ctx context.Context, t *v1alpha1.Task) (Decision, error) {
	dataClass := t.Spec.DataClass
	if dataClass == "" {
		dataClass = c.Cfg.Defaults.DataClass
	}
	class, err := c.predictClass(ctx, t)
	if err != nil {
		return Decision{}, err
	}
	if t.Spec.Source.Kind == "runlore" {
		dataClass = "internal" // the investigate template forces it (§2)
	}
	res, err := c.C.Classify(ctx, C7Request{Text: t.Spec.Text, Ref: t.Name, DataClass: dataClass})
	if err != nil || (res.Tier != "light" && res.Tier != "standard" && res.Tier != "frontier") {
		res = C7Response{Tier: "standard", Classifier: "static", Fallback: "static"} // never blocks (C7)
	}
	cl := v1alpha1.Classification{Tier: res.Tier, Classifier: res.Classifier, Fallback: res.Fallback}
	if res.Confidence > 0 {
		cl.Confidence = fmt.Sprintf("%.2f", res.Confidence)
	}
	for _, s := range res.Shadow {
		cl.Shadow = append(cl.Shadow, v1alpha1.ShadowVerdict{Classifier: s.Classifier, Tier: s.Tier, Confidence: fmt.Sprintf("%.2f", s.Confidence)})
	}
	tier := res.Tier
	if Control(t.Name, c.Cfg.Triage.ControlPercent) {
		cl.Control, tier = true, "frontier"
	}
	return Decision{Template: Matrix(class, t.Spec.Source.Kind, tier), PredictedClass: class, DataClass: dataClass,
		Budget: BudgetFor(c.Cfg, tier), Classification: cl}, nil
}

// predictClass: the schedule's class; else a maintainer-applied class:<name> label; else review.
// RunLore tasks are always review. Labels are intent, never authority (§2, §8 T3).
func (c Classify) predictClass(ctx context.Context, t *v1alpha1.Task) (string, error) {
	switch t.Spec.Source.Kind {
	case "runlore":
		return "review", nil
	case "schedule":
		if _, ok := c.Cfg.Classes[t.Spec.PredictedClass]; ok {
			return t.Spec.PredictedClass, nil
		}
		return "review", nil
	}
	if t.Spec.Issue == 0 {
		return "review", nil
	}
	iss, err := c.Forge.Issue(ctx, t.Spec.Issue)
	if err != nil {
		return "", err
	}
	for _, l := range iss.Labels {
		name, ok := strings.CutPrefix(l, "class:")
		if _, known := c.Cfg.Classes[name]; !ok || !known || name == "revert" {
			continue // revert is the factory's own class, never predicted from a label
		}
		evs, err := c.Forge.LabelEvents(ctx, t.Spec.Issue, l)
		if err != nil {
			return "", err
		}
		if len(evs) > 0 && c.Cfg.IsMaintainer(evs[len(evs)-1].Actor) {
			return name, nil
		}
	}
	return "review", nil
}
