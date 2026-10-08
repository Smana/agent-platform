// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/sanitize"
	"github.com/Smana/agent-platform/internal/factory/taskid"
)

// LabelSourceKey carries taskid.Name of a finding's dedup key on every task made from that key
// (§1): tasks of one key share the label, so a replay finds the open one and a new generation
// starts after the last task ends.
const LabelSourceKey = "agents.ogenki.io/source-key"

// Finding is what FR-9's notify.templated template renders from RunLore's payload.
type Finding struct {
	Title       string  `json:"title"`
	Verdict     string  `json:"verdict"`
	Confidence  float64 `json:"confidence"`
	AlertName   string  `json:"alert_name"`
	ResourceRef string  `json:"resource_ref"`
	Severity    string  `json:"severity"`
	Cluster     string  `json:"cluster"`
	Text        string  `json:"text"`
}

// runloreForge is the part of the forge the intake uses, as the factory App (issues:write is in
// its scope).
type runloreForge interface {
	CreateIssue(ctx context.Context, title, body string, labels []string) (int, error)
}

// RunLore is the intake of §1: RunLore POSTs a finding, and an actionable one becomes a Task and
// a proposed issue. It serves POST /intake/runlore on runlore.listen over the tailnet (FR-9's
// notify route), on every replica like the run-request API. Token re-reads the mounted bearer
// secret at each request, so a rotation needs no restart (AGENTS.md).
type RunLore struct {
	Forge     runloreForge
	Client    client.Client
	Namespace string
	Cfg       *config.Config
	Token     func() string
	Stopped   func(context.Context) bool // the stop object (§6.1): intake pauses while it holds
	Now       func() time.Time
	Errors    errorCounter
	Log       *slog.Logger
}

// NeedLeaderElection is false: every replica serves intake, and the task's name is the key, so a
// replay racing across replicas loses on AlreadyExists and opens no second issue (SC-9).
func (h *RunLore) NeedLeaderElection() bool { return false }

// Start serves POST /intake/runlore on runlore.listen until ctx ends, then drains, in the
// run-request API server's shape (internal/factory/api).
func (h *RunLore) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("POST /intake/runlore", h)
	srv := &http.Server{Addr: h.Cfg.RunLore.Listen, Handler: mux,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second) // the drain outlives the root ctx (bridgeapi's idiom)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (h *RunLore) log() *slog.Logger {
	if h.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Log
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *RunLore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := authn.Bearer(r)
	if err != nil || subtle.ConstantTimeCompare([]byte(raw), []byte(h.Token())) != 1 {
		reply(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	if h.Stopped(r.Context()) {
		reply(w, http.StatusServiceUnavailable, map[string]string{"error": "factory_stopped"})
		return
	}
	var f Finding
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&f); err != nil || f.AlertName == "" || f.ResourceRef == "" {
		reply(w, http.StatusBadRequest, map[string]string{"error": "bad_finding"})
		return
	}
	if (f.Verdict != "action_required" && f.Verdict != "action_suggested") || f.Confidence < h.Cfg.RunLore.MinConfidence {
		reply(w, http.StatusAccepted, map[string]any{"accepted": false, "reason": "below_threshold"})
		return
	}
	code, body, err := h.intake(r.Context(), f)
	if err != nil {
		if h.Errors != nil {
			h.Errors.IntakeError(r.Context(), "runlore")
		}
		h.log().Warn("runlore intake failed", "alert", f.AlertName, "resource", f.ResourceRef, "err", err)
		reply(w, http.StatusServiceUnavailable, map[string]string{"error": "intake_failed"})
		return
	}
	reply(w, code, body)
}

// labelSafe transposes every character outside the label charset ([A-Za-z0-9_.:/-]) to "_",
// so no markdown or HTML trigger reaches the public issue. See its use in intake for why this
// filter, not sanitize.Text, guards those fields (TW6).
var labelUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.:/-]`)

func labelSafe(s string) string { return labelUnsafe.ReplaceAllString(s, "_") }

// intake dedups the finding while a task for its key is open (SC-9), holds OD-9's daily cap,
// then creates the task before the issue: the task's name is the key's hash, so a replay racing
// on the other replica loses on AlreadyExists and opens no second issue.
func (h *RunLore) intake(ctx context.Context, f Finding) (int, any, error) {
	key := taskid.RunloreKey(f.AlertName, f.ResourceRef)
	group := taskid.Name(key)
	var same v1alpha1.TaskList
	if err := h.Client.List(ctx, &same, client.InNamespace(h.Namespace), client.MatchingLabels{LabelSourceKey: group}); err != nil {
		return 0, nil, err
	}
	for _, t := range same.Items {
		if !v1alpha1.TerminalPhase(t.Status.Phase) {
			return http.StatusOK, map[string]any{"duplicate": true, "task": t.Name}, nil
		}
	}
	var all v1alpha1.TaskList
	if err := h.Client.List(ctx, &all, client.InNamespace(h.Namespace)); err != nil {
		return 0, nil, err
	}
	today := 0
	for _, t := range all.Items {
		if t.Spec.Source.Kind == "runlore" && t.CreationTimestamp.UTC().Format(time.DateOnly) == h.Now().UTC().Format(time.DateOnly) {
			today++
		}
	}
	if today >= h.Cfg.RunLore.DailyCap {
		return http.StatusTooManyRequests, map[string]string{"error": "runlore_daily_cap"}, nil
	}
	text := fmt.Sprintf("# %s\n\nRunLore verdict %s (confidence %.2f) on %s, alert %s, severity %s, cluster %s.\n\n%s",
		f.Title, f.Verdict, f.Confidence, f.ResourceRef, f.AlertName, f.Severity, f.Cluster, f.Text)
	sum := sha256.Sum256([]byte(text))
	text, _ = sanitize.Text(text) // G2, R43: alert and log text are written outside the platform too
	if len(text) > h.Cfg.Caps.MaxTextBytes && h.Cfg.Caps.MaxTextBytes > 0 {
		text = strings.ToValidUTF8(text[:max(h.Cfg.Caps.MaxTextBytes-64, 0)], "") + "\n[finding truncated by the factory]"
	}
	name := taskid.Name(fmt.Sprintf("%s:gen%d", key, len(same.Items)+1))
	t := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.Namespace,
			Labels: map[string]string{LabelSourceKey: group},
			// The day bucket above reads the creation time; the apiserver sets its own on
			// create, so this only makes the injected clock decide it in tests.
			CreationTimestamp: metav1.NewTime(h.Now())},
		Spec: v1alpha1.TaskSpec{
			// Alert and log text are attacker-influenced (§8 T1): untrusted, fenced for the harness.
			Source: v1alpha1.Source{Kind: "runlore", Ref: f.AlertName + "/" + f.ResourceRef, Key: key,
				RequestedBy: "system:runlore", Trust: "untrusted", ContentSHA256: hex.EncodeToString(sum[:])},
			Repository: h.Cfg.Repository, Text: text, DataClass: "internal",
		},
	}
	if err := h.Client.Create(ctx, t); apierrors.IsAlreadyExists(err) {
		return http.StatusOK, map[string]any{"duplicate": true, "task": name}, nil // the other replica won
	} else if err != nil {
		return 0, nil, err
	}
	// The issue is public: alert, resource, verdict and the room, never the finding (R33). The
	// attacker-influenced identifiers reach it transposed to the label charset (TW6): they are
	// still there, but no markup can form around them.
	//
	// Chosen over sanitize.Text deliberately: that defuses images, "]:" definitions and raw HTML
	// but leaves an inline link — "[click](javascript:alert(1))" — intact, and these fields are
	// identifiers whose natural charset is [A-Za-z0-9_.:/-] (alert names, resource refs,
	// severities). Transposing everything else to "_" costs nothing they legitimately hold while
	// removing every markdown and HTML trigger character ("!", "[", "]", "(", "<", "`", space)
	// and every invisible code point. Verdict is already a closed set and Confidence a float.
	alert, ref, sev := labelSafe(f.AlertName), labelSafe(f.ResourceRef), labelSafe(f.Severity)
	body := fmt.Sprintf("RunLore reported **%s** on `%s` (alert `%s`, verdict `%s`, confidence %.2f).\n\n"+
		"Agent factory task `%s` investigates it. The finding and the work stay in its room: %s/r/%s (tailnet only).\n\n"+
		"Apply `factory/stop` to stop it.", sev, ref, alert, f.Verdict, f.Confidence,
		name, strings.TrimSuffix(h.Cfg.RoomsURL, "/"), name)
	n, err := h.Forge.CreateIssue(ctx, fmt.Sprintf("RunLore: %s on %s", alert, ref), body, []string{"factory/proposed"})
	if err != nil {
		return 0, nil, err
	}
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"issue": n}})
	if err := h.Client.Patch(ctx, &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.Namespace}},
		client.RawPatch(types.MergePatchType, patch)); err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"task": name, "issue": n}, nil
}
