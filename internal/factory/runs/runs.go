// SPDX-License-Identifier: Apache-2.0

// Package runs builds and drives AgentRun claims (C3). The claim is the one SP1's
// scripts/ops/k8s/agent-run.sh builds, plus the fields only the factory sets. Status has one
// writer, the composition: this package writes annotations, never status.
package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
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
	// Both set at CREATE only (R48), so a replay that finds the claim reads back what its run was
	// given, never what the replay would give a run now.
	AnnStartSeq = "agents.ogenki.io/start-seq" // the room's lastSeq when the run was created
	AnnHead     = "agents.ogenki.io/head"      // the pull request head a verifier was given

	namePrefix = runwatch.ClaimPrefix
)

// GVK is SP1's AgentRun claim: the broker's own, so the two readers of a claim never disagree.
func GVK() schema.GroupVersionKind { return runwatch.GVK() }

// Scheme registers AgentRun as an unstructured kind (the XRD has no Go types).
func Scheme(s *runtime.Scheme) {
	s.AddKnownTypeWithName(GVK(), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(GVK().GroupVersion().WithKind("AgentRunList"), &unstructured.UnstructuredList{})
}

// Spec is what the factory decides about one run. TaskText and TaskURL are exclusive, as the
// XRD's CEL requires: a URL wins. SourceURL is the GitHub issue or PR the task narrates on
// (R28), and is written as AnnTaskURL whatever the task field holds. StartSeq and Head are the
// CREATE-only annotations' sources (R48).
type Spec struct {
	RunID, TaskID, Role, Repository, BaseRef, Branch, TaskText, TaskURL, Principal, DataClass, Model, RoomRef, Queue, Traceparent, Tier string
	SourceURL, Head                                                                                                                     string
	MaxTokens, MaxMinutes, StartSeq                                                                                                     int64
	EgressProfiles                                                                                                                      []string
}

var (
	// githubURL is the one shape GitHub writes as an issue's or a PR's html_url. It is matched on
	// the raw string, so nothing url.Parse would decode or tolerate gets through: a control
	// character, a percent-escape, userinfo, a port, a query or a fragment.
	githubURL = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9-]+/[A-Za-z0-9._-]+)/(?:issues|pull)/[1-9][0-9]*$`)
	// traceparent is W3C Trace Context version 00 in lower-case hex. Any flags byte: level 2
	// adds the random-trace-id bit (0x02), and refusing it would fail every CREATE.
	traceparent = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)
)

// Validate refuses what the claim cannot carry safely. The task URL ends in the harness footer
// as `Agent-Task: <URL>` (SF), so a newline in it would forge a trailer, Agent-Run included
// (review I4); it must be an issue or a PR of the run's own repository.
func (s Spec) Validate() error {
	var errs []error
	if !envelope.ValidID(s.RunID) {
		errs = append(errs, fmt.Errorf("run id %q is not a C2 id", s.RunID))
	}
	for _, u := range []struct{ field, value string }{{"source URL", s.SourceURL}, {"task URL", s.TaskURL}} {
		if u.value == "" {
			continue
		}
		if m := githubURL.FindStringSubmatch(u.value); m == nil || !strings.EqualFold(m[1], s.Repository) {
			errs = append(errs, fmt.Errorf("%s %q is not an issue or a pull request of %s on https://github.com", u.field, u.value, s.Repository))
		}
	}
	// W3C: an all-zero trace id or parent id is invalid, and the harness would parent on nothing.
	if s.Traceparent != "" && (!traceparent.MatchString(s.Traceparent) ||
		s.Traceparent[3:35] == strings.Repeat("0", 32) || s.Traceparent[36:52] == strings.Repeat("0", 16)) {
		errs = append(errs, fmt.Errorf("traceparent %q is not W3C version 00", s.Traceparent))
	}
	if s.Tier != "" && !slices.Contains([]string{"light", "standard", "frontier"}, s.Tier) {
		errs = append(errs, fmt.Errorf("tier %q is not light, standard or frontier", s.Tier))
	}
	return errors.Join(errs...)
}

// Name is the claim's name for a run id.
func Name(runID string) string { return namePrefix + runID }

// Build is the claim for s. It does not validate: Create does, before the claim exists.
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
	u.SetGroupVersionKind(GVK())
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
	// All are written at CREATE only: the phase-5 patch limit never has to admit them.
	ann := map[string]string{}
	if s.Traceparent != "" {
		ann[AnnTraceparent] = s.Traceparent
	}
	if s.SourceURL != "" {
		ann[AnnTaskURL] = s.SourceURL
	}
	// A start seq of 0 is an empty room's lastSeq, and 0 is what an absent annotation reads back.
	if s.StartSeq != 0 {
		ann[AnnStartSeq] = strconv.FormatInt(s.StartSeq, 10)
	}
	if s.Head != "" {
		ann[AnnHead] = s.Head
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
	Head                                                                              string
	Tokens, MaxTokens, StartSeq                                                       int64
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
		PullRequest: str("status", "pullRequest"), Revoked: u.GetAnnotations()[AnnRevoked], Head: u.GetAnnotations()[AnnHead]}
	// The CREATE-only start seq (R48): absent means the room was empty when the run was created.
	if v, err := strconv.ParseInt(u.GetAnnotations()[AnnStartSeq], 10, 64); err == nil {
		r.StartSeq = v
	}
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
	u.SetGroupVersionKind(GVK())
	return u
}

func named(id string) *unstructured.Unstructured {
	u := empty()
	u.SetNamespace(Namespace)
	u.SetName(Name(id))
	return u
}

// Create validates s, then creates its claim: the CREATE-only values are checked once, here.
// An AlreadyExists for the same run (R48's replay, checked against the claim's CREATE-only
// values) is success, not an error: the caller's own first attempt landed.
func (c Client) Create(ctx context.Context, s Spec) error {
	if err := s.Validate(); err != nil {
		return fmt.Errorf("create run %s: %w", s.RunID, err)
	}
	if err := c.C.Create(ctx, Build(s)); err != nil {
		if apierrors.IsAlreadyExists(err) && c.replayOf(ctx, s) {
			return nil
		}
		return fmt.Errorf("create run %s: %w", s.RunID, err)
	}
	return nil
}

// replayOf reports whether the claim under s's id is this same run. The derived id is the
// identity (R48), so equality is what only CREATE could have written: start seq and head.
// Everything else on a live claim is the composition's or the meter's to change.
func (c Client) replayOf(ctx context.Context, s Spec) bool {
	u := empty()
	if err := c.C.Get(ctx, types.NamespacedName{Namespace: Namespace, Name: Name(s.RunID)}, u); err != nil {
		return false
	}
	a := u.GetAnnotations()
	start := ""
	if s.StartSeq != 0 {
		start = strconv.FormatInt(s.StartSeq, 10)
	}
	return a[AnnStartSeq] == start && a[AnnHead] == s.Head
}

// validID refuses an id that names no run, before it reaches the API as xplane-run-<id>.
func validID(op, id string) error {
	if !envelope.ValidID(id) {
		return fmt.Errorf("%s run %q: not a C2 id", op, id)
	}
	return nil
}

// Get reads run id; found is false when it does not exist.
func (c Client) Get(ctx context.Context, id string) (Run, bool, error) {
	if err := validID("get", id); err != nil {
		return Run{}, false, err
	}
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
	l.SetGroupVersionKind(GVK().GroupVersion().WithKind("AgentRunList"))
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
// anything else from the factory's ServiceAccount. It refuses the CREATE-only keys itself, so
// that rule is never their only guard.
func (c Client) Annotate(ctx context.Context, id string, kv map[string]string) error {
	if err := validID("annotate", id); err != nil {
		return err
	}
	for _, k := range []string{AnnTaskURL, AnnTraceparent, AnnStartSeq, AnnHead} {
		if _, ok := kv[k]; ok {
			return fmt.Errorf("annotate run %s: %s is set at CREATE only", id, k)
		}
	}
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
	if err := validID("delete", id); err != nil {
		return err
	}
	if err := client.IgnoreNotFound(c.C.Delete(ctx, named(id))); err != nil {
		return fmt.Errorf("delete run %s: %w", id, err)
	}
	return nil
}
