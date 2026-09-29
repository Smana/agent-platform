// SPDX-License-Identifier: Apache-2.0

// Package runs builds and drives AgentRun claims (C3). The claim is the one SP1's
// scripts/ops/k8s/agent-run.sh builds, plus the fields only the factory sets. Status has one
// writer, the composition: this package writes annotations, never status.
package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/runwatch"
)

// The claim's namespace, labels, annotations, principal and queues. Namespace and AnnRevoked
// are the broker's own (runwatch), so the two readers of a claim never disagree.
const (
	Namespace        = runwatch.Namespace
	LabelTask        = "agents.ogenki.io/task"
	LabelRole        = "agents.ogenki.io/role"
	LabelPrincipal   = "agents.ogenki.io/principal" // ":" is not valid in a label value: written "."
	AnnUsage         = "agents.ogenki.io/usage-tokens"
	AnnPullRequest   = "agents.ogenki.io/pull-request"
	AnnRevoked       = runwatch.RevokedAnnotation
	PrincipalFactory = "system:factory"
	QueueFactory     = "factory"
	QueueInteractive = "interactive"
	AnnTraceparent   = "agents.ogenki.io/traceparent" // W3C, set at CREATE; the composition hands it to the harness (R46)
	LabelTier        = "agents.ogenki.io/tier"        // fixed per run, never re-routed within it (R47)
	AnnTaskURL       = "agents.ogenki.io/task-url"    // set at CREATE; the harness footer's Agent-Task for a text task (SF)

	namePrefix = "xplane-run-"
)

// GVK is SP1's AgentRun claim.
var GVK = schema.GroupVersionKind{Group: "cloud.ogenki.io", Version: "v1alpha1", Kind: "AgentRun"}

// Scheme registers AgentRun as an unstructured kind (the XRD has no Go types).
func Scheme(s *runtime.Scheme) {
	s.AddKnownTypeWithName(GVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(GVK.GroupVersion().WithKind("AgentRunList"), &unstructured.UnstructuredList{})
}

// Spec is what the factory decides about one run. TaskText and TaskURL are exclusive, as the
// XRD's CEL requires: a URL wins. SourceURL is the GitHub issue or PR the task narrates on
// (R28), and is written as AnnTaskURL whatever the task field holds.
type Spec struct {
	RunID, TaskID, Role, Repository, BaseRef, Branch, TaskText, TaskURL, Principal, DataClass, Model, RoomRef, Queue, Traceparent, Tier string
	SourceURL                                                                                                                           string
	MaxTokens, MaxMinutes                                                                                                               int64
	EgressProfiles                                                                                                                      []string
}

// Name is the claim's name for a run id.
func Name(runID string) string { return namePrefix + runID }

// Build is the claim for s.
func Build(s Spec) *unstructured.Unstructured {
	task := map[string]any{"text": s.TaskText}
	if s.TaskURL != "" {
		task = map[string]any{"url": s.TaskURL}
	}
	spec := map[string]any{
		"role": s.Role, "repository": s.Repository, "principal": s.Principal, "dataClass": s.DataClass,
		"size": "small", "model": s.Model, "baseRef": s.BaseRef, "branch": s.Branch, "task": task,
		"budget": map[string]any{"maxTokens": s.MaxTokens, "maxMinutes": s.MaxMinutes},
	}
	if s.RoomRef != "" {
		spec["roomRef"] = s.RoomRef
	}
	if s.Queue != "" {
		spec["queueName"] = s.Queue
	}
	if len(s.EgressProfiles) > 0 {
		profiles := make([]any, 0, len(s.EgressProfiles))
		for _, p := range s.EgressProfiles {
			profiles = append(profiles, p)
		}
		spec["egress"] = map[string]any{"profiles": profiles}
	}
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(GVK)
	u.SetNamespace(Namespace)
	u.SetName(Name(s.RunID))
	labels := map[string]string{LabelRole: s.Role, LabelPrincipal: strings.ReplaceAll(s.Principal, ":", ".")}
	if s.TaskID != "" {
		labels[LabelTask] = s.TaskID // the composition copies it onto every composed object
	}
	if s.Tier != "" {
		labels[LabelTier] = s.Tier
	}
	u.SetLabels(labels)
	// Both are written at CREATE only: the phase-5 patch limit never has to admit them.
	ann := map[string]string{}
	if s.Traceparent != "" {
		ann[AnnTraceparent] = s.Traceparent
	}
	if s.SourceURL != "" {
		ann[AnnTaskURL] = s.SourceURL
	}
	if len(ann) > 0 {
		u.SetAnnotations(ann)
	}
	return u
}

// Terminal reports whether an AgentRun phase is final: the broker's own set.
func Terminal(phase string) bool { return runwatch.Terminal(phase) }

// Run is the part of a claim the factory reads back.
type Run struct {
	ID, TaskID, Role, Principal, Phase, Reason, PullRequest, Revoked, Branch, RoomRef string
	Tokens, MaxTokens                                                                 int64
	Created, Finished                                                                 time.Time
}

// FromUnstructured reads a claim. ok is false for one that is not a run: not named
// xplane-run-<C2 id>, or outside Namespace, as the broker's runwatch decides.
func FromUnstructured(u *unstructured.Unstructured) (Run, bool) {
	id, ok := strings.CutPrefix(u.GetName(), namePrefix)
	if !ok || !envelope.ValidID(id) || u.GetNamespace() != Namespace {
		return Run{}, false
	}
	str := func(path ...string) string { v, _, _ := unstructured.NestedString(u.Object, path...); return v }
	r := Run{ID: id, TaskID: u.GetLabels()[LabelTask], Created: u.GetCreationTimestamp().Time,
		Role: str("spec", "role"), Principal: str("spec", "principal"), Branch: str("spec", "branch"),
		RoomRef: str("spec", "roomRef"), Phase: str("status", "phase"), Reason: str("status", "reason"),
		PullRequest: str("status", "pullRequest"), Revoked: u.GetAnnotations()[AnnRevoked]}
	r.MaxTokens, _, _ = unstructured.NestedInt64(u.Object, "spec", "budget", "maxTokens")
	// The annotation is the run's monotonic high-water mark (R12); status is the fallback.
	if v, err := strconv.ParseInt(u.GetAnnotations()[AnnUsage], 10, 64); err == nil {
		r.Tokens = v
	} else {
		r.Tokens, _, _ = unstructured.NestedInt64(u.Object, "status", "usage", "tokens")
	}
	if f := str("status", "finishedAt"); f != "" {
		r.Finished, _ = time.Parse(time.RFC3339, f)
	}
	return r, true
}

// Client creates, reads, annotates and deletes claims in Namespace.
type Client struct{ C client.Client }

func empty() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(GVK)
	return u
}

func named(id string) *unstructured.Unstructured {
	u := empty()
	u.SetNamespace(Namespace)
	u.SetName(Name(id))
	return u
}

// Create creates the claim for s.
func (c Client) Create(ctx context.Context, s Spec) error {
	if err := c.C.Create(ctx, Build(s)); err != nil {
		return fmt.Errorf("create run %s: %w", s.RunID, err)
	}
	return nil
}

// Get reads run id; found is false when it does not exist.
func (c Client) Get(ctx context.Context, id string) (Run, bool, error) {
	u := empty()
	err := c.C.Get(ctx, types.NamespacedName{Namespace: Namespace, Name: Name(id)}, u)
	if apierrors.IsNotFound(err) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("get run %s: %w", id, err)
	}
	r, ok := FromUnstructured(u)
	return r, ok, nil
}

// List reads every run in Namespace.
func (c Client) List(ctx context.Context) ([]Run, error) {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(GVK.GroupVersion().WithKind("AgentRunList"))
	if err := c.C.List(ctx, l, client.InNamespace(Namespace)); err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	out := make([]Run, 0, len(l.Items))
	for i := range l.Items {
		if r, ok := FromUnstructured(&l.Items[i]); ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// Annotate merge-patches annotations only: the Kyverno patch-limit rule (phase 5) refuses
// anything else from the factory's ServiceAccount.
func (c Client) Annotate(ctx context.Context, id string, kv map[string]string) error {
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": kv}})
	if err != nil {
		return fmt.Errorf("annotate run %s: %w", id, err)
	}
	if err := c.C.Patch(ctx, named(id), client.RawPatch(types.MergePatchType, body)); err != nil {
		return fmt.Errorf("annotate run %s: %w", id, err)
	}
	return nil
}

// Delete deletes run id; a run already gone is not an error.
func (c Client) Delete(ctx context.Context, id string) error {
	if err := client.IgnoreNotFound(c.C.Delete(ctx, named(id))); err != nil {
		return fmt.Errorf("delete run %s: %w", id, err)
	}
	return nil
}
