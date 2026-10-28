// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/forge"
	"github.com/Smana/maku/internal/factory/forge/forgetest"
	"github.com/Smana/maku/internal/redact"
)

var (
	redactorOnce sync.Once
	redactorVal  *redact.Redactor
	redactorErr  error
)

func testRedactor(t *testing.T) *redact.Redactor {
	t.Helper()
	redactorOnce.Do(func() { redactorVal, redactorErr = redact.New() })
	if redactorErr != nil {
		t.Fatal(redactorErr)
	}
	return redactorVal
}

const proposalOK = "docs(alerts): the crash-loop rule fires on a completed Job\n" +
	"## What is wrong\nThe rule counts Job pods.\n## What it should be\nIt skips them.\n## Acceptance check\nThe alert stays quiet."

// investigated is a Done investigate task of issue 7, with a proposal.
func investigated(name string) *v1alpha1.Task {
	return &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agent-system", CreationTimestamp: metav1.NewTime(t0),
		Labels: map[string]string{v1alpha1.LabelIssue: "7"}},
		Spec: v1alpha1.TaskSpec{Source: v1alpha1.Source{Kind: "api", RequestedBy: "system:runlore"}, Repository: "Smana/cloud-native-ref",
			Issue: 7, Template: "investigate", DataClass: "internal"},
		Status: v1alpha1.TaskStatus{Phase: v1alpha1.PhaseDone}}
}

// publishing is a poller whose proposal reader answers text, with 7 labelled factory/publish by actors.
func publishing(t *testing.T, text string, tk *v1alpha1.Task, actors ...string) (*IssuePoller, *forgetest.Fake, client.Client) {
	t.Helper()
	f := forgetest.NewFake()
	var evs []forge.LabelEvent
	for i, a := range actors {
		evs = append(evs, forge.LabelEvent{Actor: a, Label: LabelPublish, At: t0.Add(time.Duration(i) * time.Minute)})
	}
	f.SetEvents(7, evs...)
	f.SetLabeled(LabelPublish, forge.Item{Number: 7})
	f.SetIssue(forge.Issue{Number: 7, Title: "RunLore: A on r", Body: "stub"})
	p, c := poller(t, f, tk)
	p.Cfg.Maintainers = []string{"Smana"}
	p.Now = func() time.Time { return t0.Add(time.Hour) }
	p.Redact = testRedactor(t)
	p.Proposal = func(context.Context, *v1alpha1.Task) (string, error) {
		if text == "" {
			return "", ErrNoProposal
		}
		return text, nil
	}
	return p, f, c
}

func getTask(t *testing.T, c client.Client, name string) v1alpha1.Task {
	t.Helper()
	var tk v1alpha1.Task
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "agent-system", Name: name}, &tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

// RA9, R7, offline proof 3: a maintainer's factory/publish files the proposal once, unlabelled,
// with a footer, records it on the task and answers on the stub.
func TestPublishFilesTheProposalOnce(t *testing.T) {
	p, f, c := publishing(t, proposalOK, investigated("3buqdlot"), "Smana")
	if err := p.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.Created(); len(got) != 1 || !strings.HasPrefix(got[0], "docs(alerts): the crash-loop rule fires on a completed Job\n") ||
		!strings.Contains(got[0], "\n\n---\nPublished by @Smana from agent factory task `3buqdlot`.\nlabels: ") || !strings.HasSuffix(got[0], "labels: factory/published-from:3buqdlot") {
		t.Fatalf("the draft's title, a footer and only the marker that makes a retry find it, never factory/ready: %q", got)
	}
	tk := getTask(t, c, "3buqdlot")
	if pub := tk.Status.Published; pub == nil || pub.Issue != 501 || pub.By != "smana" || !pub.At.Time.Equal(t0.Add(time.Hour)) ||
		tk.Labels[v1alpha1.LabelPublishedIssue] != "501" {
		t.Fatalf("%+v %v", tk.Status.Published, tk.Labels)
	}
	if cs := strings.Join(f.Comments(7), "\n"); !strings.Contains(cs, "Published as #501 by @smana") {
		t.Fatalf("%q", cs)
	}
	if !slices.Contains(f.Removed(7), LabelPublish) {
		t.Fatalf("the label stays: %v", f.Removed(7))
	}
	// the same label again: a pointer, no second issue
	f.SetLabeled(LabelPublish, forge.Item{Number: 7})
	if err := p.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.Created()) != 1 || len(f.Removed(7)) != 2 {
		t.Fatalf("%d issues, removed %v", len(f.Created()), f.Removed(7))
	}
}

// A replay on a task already published: a pointer to the issue, nothing created.
func TestPublishReplayNamesTheIssue(t *testing.T) {
	tk := investigated("3buqdlot")
	tk.Status.Published = &v1alpha1.Published{Issue: 42, By: "smana", At: metav1.NewTime(t0)}
	p, f, _ := publishing(t, proposalOK, tk, "Smana")
	if err := p.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if cs := strings.Join(f.Comments(7), "\n"); len(f.Created()) != 0 || !strings.Contains(cs, "#42") {
		t.Fatalf("%v %q", f.Created(), cs)
	}
}

// The refusals each answer once, consume the label and create nothing.
func TestPublishRefusals(t *testing.T) {
	aws := "AKIA" + "Z3QPLR7MXN2KWT5V" // an access key id, built from halves so no literal sits in the source
	for name, c := range map[string]struct {
		actors []string
		tk     func(*v1alpha1.Task)
		text   string
		want   string
	}{
		"a stranger":                   {[]string{"mallory"}, nil, proposalOK, "only a maintainer's factory/publish"},
		"a maintainer then a stranger": {[]string{"Smana", "mallory"}, nil, proposalOK, "only a maintainer's factory/publish"},
		"a task still running":         {[]string{"Smana"}, func(t *v1alpha1.Task) { t.Status.Phase = v1alpha1.PhaseImplementing }, proposalOK, "still running"},
		"a stopped task":               {[]string{"Smana"}, func(t *v1alpha1.Task) { t.Status.Phase = v1alpha1.PhaseStopped }, proposalOK, "made no proposal"},
		"a task of another template":   {[]string{"Smana"}, func(t *v1alpha1.Task) { t.Spec.Template = "pair" }, proposalOK, "made no proposal"},
		"no proposal":                  {[]string{"Smana"}, nil, "", "made no proposal"},
		"a bad title":                  {[]string{"Smana"}, nil, "Fix it\n## What is wrong\nx\n## What it should be\ny\n## Acceptance check\nz", "issue template"},
		"no sections":                  {[]string{"Smana"}, nil, "docs(a): b\njust text", "issue template"},
		"a secret":                     {[]string{"Smana"}, nil, strings.Replace(proposalOK, "The rule counts Job pods.", "key "+aws+" and secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYzZ3qLpTnVb", 1), "looks like a secret"},
	} {
		t.Run(name, func(t *testing.T) {
			tk := investigated("3buqdlot")
			if c.tk != nil {
				c.tk(tk)
			}
			p, f, cl := publishing(t, c.text, tk, c.actors...)
			if err := p.Poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			cs := strings.Join(f.Comments(7), "\n")
			if len(f.Created()) != 0 || !strings.Contains(cs, "did not publish #7's proposal") || !strings.Contains(cs, c.want) ||
				!slices.Contains(f.Removed(7), LabelPublish) {
				t.Fatalf("%d issues, comments %q, removed %v", len(f.Created()), cs, f.Removed(7))
			}
			if got := getTask(t, cl, "3buqdlot"); got.Status.Published != nil {
				t.Fatalf("a refusal records nothing: %+v", got.Status.Published)
			}
		})
	}
}

// A write check that fails leaves the label for the next poll, as factory/ready does.
func TestPublishNeedsTheWriteCheck(t *testing.T) {
	p, f, _ := publishing(t, proposalOK, investigated("3buqdlot"), "Smana")
	p.CanWrite = func(context.Context, string) (bool, error) { return false, nil }
	if err := p.Poll(t.Context()); err != nil || len(f.Created()) != 0 || !strings.Contains(strings.Join(f.Comments(7), ""), "only a maintainer") {
		t.Fatalf("%v %v", err, f.Created())
	}
	p, f, _ = publishing(t, proposalOK, investigated("3buqdlot"), "Smana")
	p.CanWrite = func(context.Context, string) (bool, error) { return false, errors.New("github down") }
	if err := p.Poll(t.Context()); err == nil || len(f.Created()) != 0 || len(f.Comments(7)) != 0 || len(f.Removed(7)) != 0 {
		t.Fatalf("no answer is no refusal: %v", err)
	}
}

// R8, offline proof 3: a hostile proposal is filed inert: no link, no raw HTML, no bidi character.
func TestPublishFilesAnInertBody(t *testing.T) {
	hostile := "docs(alerts): fix [the rule](https://evil.example/t)\n## What is wrong\nsee [x](https://evil.example/x) www.evil.example <img src=x onerror=1> \u202eevil\n" +
		"## What it should be\nhttps://evil.example/y\n## Acceptance check\nz"
	p, f, _ := publishing(t, hostile, investigated("3buqdlot"), "Smana")
	if err := p.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	created := f.Created()
	if len(created) != 1 {
		t.Fatalf("%v", created)
	}
	if strings.ContainsRune(created[0], '\u202e') || strings.Contains(created[0], "](") || strings.Contains(created[0], "<img") ||
		strings.Contains(created[0], " https://evil.example/y") || strings.Contains(created[0], " www.evil.example") {
		t.Fatalf("%q", created[0])
	}
	if !strings.Contains(created[0], "`https://evil.example/y`") {
		t.Fatalf("%q", created[0])
	}
}

// RA9, R9: a fix task's origin is the factory's record, never what the issue says.
func TestOriginOfAStartedIssue(t *testing.T) {
	published := func(name, labelled string, recorded int) *v1alpha1.Task {
		tk := investigated(name)
		tk.Labels[v1alpha1.LabelPublishedIssue] = labelled
		tk.Status.Published = &v1alpha1.Published{Issue: recorded, By: "smana", At: metav1.NewTime(t0)}
		return tk
	}
	for name, c := range map[string]struct {
		issue int
		body  string
		objs  []client.Object
		want  string
	}{
		"the published issue":              {42, "fix it", []client.Object{published("3buqdlot", "42", 42)}, "3buqdlot"},
		"an issue that names an origin":    {43, "origin: 3buqdlot\nFollows agent factory task 3buqdlot", []client.Object{published("3buqdlot", "42", 42)}, ""},
		"a label the record does not back": {42, "fix it", []client.Object{published("3buqdlot", "42", 41)}, ""},
		"a label with no record": {42, "fix it", func() []client.Object {
			tk := investigated("3buqdlot")
			tk.Labels[v1alpha1.LabelPublishedIssue] = "42"
			return []client.Object{tk}
		}(), ""},
		"a task of another repository's issue": {42, "fix it", func() []client.Object {
			tk := published("3buqdlot", "42", 42)
			tk.Spec.Repository = "Smana/other"
			return []client.Object{tk}
		}(), ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := forgetest.NewFake()
			labelled(f, c.issue, "Smana")
			f.SetIssue(forge.Issue{Number: c.issue, Title: "docs(alerts): x", Body: c.body, Author: "ogenki-agent-factory[bot]"})
			p, cl := poller(t, f, c.objs...)
			p.Cfg.Repositories = []config.Repository{{Name: "Smana/cloud-native-ref", Maintainers: []string{"Smana"}, TriggerLabel: "factory/ready"},
				{Name: "Smana/other", Maintainers: []string{"Smana"}, TriggerLabel: "factory/ready"}}
			p.Repository = "Smana/cloud-native-ref"
			if err := p.Poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			var l v1alpha1.TaskList
			if err := cl.List(t.Context(), &l, client.MatchingLabels{v1alpha1.LabelIssue: strconv.Itoa(c.issue)}); err != nil {
				t.Fatal(err)
			}
			var started *v1alpha1.Task
			for i := range l.Items {
				if l.Items[i].Spec.Source.Kind == "issue" {
					started = &l.Items[i]
				}
			}
			if started == nil || started.Spec.Origin != c.want {
				t.Fatalf("%+v", started)
			}
		})
	}
}

// A crash between CreateIssue and the record, or a record that failed, leaves an issue the label
// still asks for: the retry reuses it, and files no second one.
func TestPublishReusesTheIssueAnEarlierAttemptFiled(t *testing.T) {
	p, f, c := publishing(t, proposalOK, investigated("3buqdlot"), "Smana")
	f.SetIssue(forge.Issue{Number: 88, Title: "docs(alerts): x", Author: "ogenki-agent-factory[bot]", Labels: []string{"factory/published-from:3buqdlot"}})
	f.SetIssue(forge.Issue{Number: 89, Title: "another task's", Labels: []string{"factory/published-from:other000"}})
	if err := p.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	tk := getTask(t, c, "3buqdlot")
	if len(f.Created()) != 0 || tk.Status.Published == nil || tk.Status.Published.Issue != 88 || tk.Labels[v1alpha1.LabelPublishedIssue] != "88" {
		t.Fatalf("%v %+v %v", f.Created(), tk.Status.Published, tk.Labels)
	}
	if cs := strings.Join(f.Comments(7), "\n"); !strings.Contains(cs, "Published as #88") || !slices.Contains(f.Removed(7), LabelPublish) {
		t.Fatalf("%q %v", cs, f.Removed(7))
	}
}

// The kill switch pauses publishing, as it pauses factory/ready: the label waits.
func TestPublishWaitsOutTheKillSwitch(t *testing.T) {
	p, f, _ := publishing(t, proposalOK, investigated("3buqdlot"), "Smana")
	p.Stopped = func(context.Context) bool { return true }
	if err := p.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.Created()) != 0 || len(f.Comments(7)) != 0 || len(f.Removed(7)) != 0 {
		t.Fatalf("%v %v %v", f.Created(), f.Comments(7), f.Removed(7))
	}
}

// Without a redactor nothing is published: no scan, no publication.
func TestPublishWithoutARedactorRefuses(t *testing.T) {
	p, f, _ := publishing(t, proposalOK, investigated("3buqdlot"), "Smana")
	p.Redact = nil
	if err := p.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if cs := strings.Join(f.Comments(7), "\n"); len(f.Created()) != 0 || !strings.Contains(cs, "looks like a secret") {
		t.Fatalf("%v %q", f.Created(), cs)
	}
}

// A marker label anyone can apply by hand is no record: only an issue the factory's own App filed
// is reused, so nobody can point a publish at an issue of their choosing.
func TestPublishIgnoresAMarkerFiledBySomeoneElse(t *testing.T) {
	p, f, c := publishing(t, proposalOK, investigated("3buqdlot"), "Smana")
	f.SetIssue(forge.Issue{Number: 88, Title: "x", Author: "mallory", Labels: []string{"factory/published-from:3buqdlot"}})
	if err := p.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	tk := getTask(t, c, "3buqdlot")
	if len(f.Created()) != 1 || tk.Status.Published == nil || tk.Status.Published.Issue == 88 {
		t.Fatalf("%d filed, %+v", len(f.Created()), tk.Status.Published)
	}
}
