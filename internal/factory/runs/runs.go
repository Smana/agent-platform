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

	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/runwatch"
	"github.com/Smana/maku/internal/version"
)

// The claim's labels, annotations, principal and queues. The namespace and AnnRevoked are the
// broker's own (runwatch), so the two readers of a claim never disagree.
const (
	LabelTask        = "agents.ogenki.io/task"
	LabelRole        = "agents.ogenki.io/role"
	LabelPrincipal   = "agents.ogenki.io/principal" // ":" is not valid in a label value: written "."
	AnnUsage         = "agents.ogenki.io/usage-tokens"
	AnnPullRequest   = "agents.ogenki.io/pull-request"
	AnnRevoked       = runwatch.RevokedAnnotation
	PrincipalFactory = config.PrincipalFactory
	QueueFactory     = "factory"
	QueueInteractive = "interactive"
	AnnTraceparent   = "agents.ogenki.io/traceparent" // W3C, set at CREATE; the composition hands it to the harness (R46)
	LabelTier        = "agents.ogenki.io/tier"        // fixed per run, never re-routed within it (R47)
	AnnTaskURL       = "agents.ogenki.io/task-url"    // set at CREATE; the harness footer's Agent-Task for a text task (SF)
	// Both set at CREATE only (R48), so a replay that finds the claim reads back what its run was
	// given, never what the replay would give a run now.
	AnnStartSeq = "agents.ogenki.io/start-seq" // the room's lastSeq when the run was created
	AnnHead     = "agents.ogenki.io/head"      // the pull request head a verifier was given
	// The run API's fingerprint of the request that made the run, so a replay of its
	// Idempotency-Key with another body is told apart from a retry. CREATE only.
	AnnRequestHash = "agents.ogenki.io/request-hash"
	// The person the run is charged to beside the fleet (v2 per-person caps D2): the meter
	// settles its tokens to both keys. CREATE only; absent, the run's principal pays alone.
	AnnChargedTo = "agents.ogenki.io/charged-to"
	// The run's own USD cap in micro-USD (F4, C-1): an api task's run is held to a third of its
	// ceiling, which AgentRun's spec.budget (maxTokens only) cannot carry. The meter reads it
	// beside its tier's cap and takes the lower. CREATE only; absent, the tier's cap alone applies.
	AnnMaxMicroUSD = "agents.ogenki.io/max-micro-usd"
	// The factory build that created the claim (R10): version.Version, CREATE only, so a replay
	// that finds the claim reads back the build that made it, not its own.
	AnnFactoryVersion = "agents.ogenki.io/factory-version"
)

// The AgentRun's status.reason for a run's pod (disruption design §3). The factory resumes a run
// lost to its infrastructure (Disrupted, PodLost), never one whose pod failed on its own.
const (
	ReasonDisrupted = "Disrupted"
	ReasonPodLost   = "PodLost"
	ReasonPodFailed = "PodFailed"
)

// Namespace is the claims' namespace: the broker's own (runwatch), so the two readers of a
// claim never disagree.
func Namespace() string { return runwatch.Namespace() }

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
	SourceURL, Head, RequestHash, ChargedTo                                                                                             string
	MaxTokens, MaxMinutes, StartSeq, MaxMicroUSD                                                                                        int64
	EgressProfiles                                                                                                                      []string
	// Suspendable is spec.suspendable (v2 suspend), written only when true: it is immutable.
	Suspendable bool
	// Loop is spec.loop (loops design L2, L3), written only when set: it is immutable.
	Loop *Loop
	// Checks is spec.loop.checks (v3 feature 3), written only when true, so every other claim is
	// today's byte for byte. It joins Loop's keys in the one spec.loop. The composition turns it
	// into the harness's AGENT_CHECKS=1.
	Checks bool
	// Search is spec.loop.search (v3 feature 5), written only when true, beside Loop's and Checks'
	// keys. The composition turns it into the harness's AGENT_SEARCH=1.
	Search bool
}

// Loop is the claim's spec.loop; JudgeModel "" writes no judgeModel, which the composition reads
// as none.
type Loop struct {
	StallSteps, Continuations int32
	JudgeModel                string
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
func Name(runID string) string { return runwatch.ClaimPrefix() + runID }

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
	if s.Suspendable {
		spec["suspendable"] = true
	}
	if l := s.Loop; l != nil || s.Checks || s.Search {
		loop := map[string]any{}
		if l != nil {
			loop["stallSteps"], loop["continuations"] = int64(l.StallSteps), int64(l.Continuations)
			if l.JudgeModel != "" {
				loop["judgeModel"] = l.JudgeModel
			}
		}
		if s.Checks {
			loop["checks"] = true
		}
		if s.Search {
			loop["search"] = true
		}
		spec["loop"] = loop
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
	u.SetNamespace(Namespace())
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
	ann := map[string]string{AnnFactoryVersion: version.Version}
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
	if s.RequestHash != "" {
		ann[AnnRequestHash] = s.RequestHash
	}
	if s.ChargedTo != "" {
		ann[AnnChargedTo] = s.ChargedTo
	}
	if s.MaxMicroUSD > 0 {
		ann[AnnMaxMicroUSD] = strconv.FormatInt(s.MaxMicroUSD, 10)
	}
	u.SetAnnotations(ann)
	return u
}

// Terminal reports whether an AgentRun phase is final: the broker's own set.
func Terminal(phase string) bool { return runwatch.Terminal(phase) }

// Run is the part of a claim the factory reads back.
type Run struct {
	ID, TaskID, Role, Principal, Phase, Reason, PullRequest, Revoked, Branch, RoomRef, Repository string
	Head, TaskText, RequestHash, Tier, Model, ChargedTo                                           string
	// What the run ran on (R10): the factory build that created the claim, the
	// CompositionRevision Crossplane bound it to, and the harness image the composition read
	// from its pod. Empty until known.
	FactoryVersion, CompositionRevision, HarnessImage string
	Tokens, MaxTokens, StartSeq, MaxMicroUSD          int64
	Created, Finished                                 time.Time
	// Deleting: the claim carries a deletion timestamp. Its finalizers can keep it listed, in
	// whatever phase it had, long after the run itself was stopped (#55).
	Deleting bool
}

// FromUnstructured reads a claim. ok is false for one that is not a run: not named
// <prefix><C2 id> (runs.namePrefix), or outside runs.namespace, as the broker's runwatch decides.
func FromUnstructured(u *unstructured.Unstructured) (Run, bool) {
	id, ok := strings.CutPrefix(u.GetName(), runwatch.ClaimPrefix())
	if !ok || !envelope.ValidID(id) || u.GetNamespace() != Namespace() {
		return Run{}, false
	}
	str := func(path ...string) string { v, _, _ := unstructured.NestedString(u.Object, path...); return v }
	r := Run{ID: id, TaskID: u.GetLabels()[LabelTask], Created: u.GetCreationTimestamp().Time, Deleting: u.GetDeletionTimestamp() != nil,
		Role: str("spec", "role"), Principal: str("spec", "principal"), Branch: str("spec", "branch"),
		RoomRef: str("spec", "roomRef"), Repository: str("spec", "repository"), Phase: str("status", "phase"), Reason: str("status", "reason"),
		PullRequest: str("status", "pullRequest"), Revoked: u.GetAnnotations()[AnnRevoked], Head: u.GetAnnotations()[AnnHead],
		RequestHash: u.GetAnnotations()[AnnRequestHash], Tier: u.GetLabels()[LabelTier], Model: str("spec", "model"),
		TaskText: str("spec", "task", "text"), ChargedTo: u.GetAnnotations()[AnnChargedTo],
		FactoryVersion: u.GetAnnotations()[AnnFactoryVersion], CompositionRevision: str("spec", "crossplane", "compositionRevisionRef", "name"),
		HarnessImage: str("status", "harnessImage")}
	// The CREATE-only start seq (R48): absent means the room was empty when the run was created.
	if v, err := strconv.ParseInt(u.GetAnnotations()[AnnStartSeq], 10, 64); err == nil {
		r.StartSeq = v
	}
	r.MaxTokens, _, _ = unstructured.NestedInt64(u.Object, "spec", "budget", "maxTokens")
	// Absent is 0; present but unreadable or not positive is -1, so the meter can tell a run
	// that never had a cap from one whose cap was damaged: both are revoked when sponsored.
	if raw, ok := u.GetAnnotations()[AnnMaxMicroUSD]; ok {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
			r.MaxMicroUSD = v
		} else {
			r.MaxMicroUSD = -1
		}
	}
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
	u.SetNamespace(Namespace())
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
	// A claim that carries spec.loop is created under Strict field validation: a host XRD without
	// spec.loop would prune it and run loop-off while the Task records the policy; refuse instead.
	var opts []client.CreateOption
	if s.Loop != nil || s.Checks || s.Search {
		opts = append(opts, client.FieldValidation("Strict"))
	}
	if err := c.C.Create(ctx, Build(s), opts...); err != nil {
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
	if err := c.C.Get(ctx, types.NamespacedName{Namespace: Namespace(), Name: Name(s.RunID)}, u); err != nil {
		return false
	}
	a := u.GetAnnotations()
	start := ""
	if s.StartSeq != 0 {
		start = strconv.FormatInt(s.StartSeq, 10)
	}
	return a[AnnStartSeq] == start && a[AnnHead] == s.Head
}

// validID refuses an id that names no run, before it reaches the API as <prefix><id>.
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
	err := c.C.Get(ctx, types.NamespacedName{Namespace: Namespace(), Name: Name(id)}, u)
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
	if err := c.C.List(ctx, l, client.InNamespace(Namespace())); err != nil {
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
	for _, k := range []string{AnnTaskURL, AnnTraceparent, AnnStartSeq, AnnHead, AnnRequestHash, AnnChargedTo, AnnFactoryVersion, AnnMaxMicroUSD} {
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
