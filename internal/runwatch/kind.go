// SPDX-License-Identifier: Apache-2.0

package runwatch

import (
	"errors"
	"fmt"
	"regexp"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Kind is where AgentRuns live and how they are named: cloud-native-ref's Crossplane XR by
// default, agent-platform's own CRD (run-controller, adoption 3a) when configured.
type Kind struct {
	Group      string `json:"group,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	NamePrefix string `json:"namePrefix,omitempty"`
}

// DefaultKind is cloud-native-ref's: SP1's XRD, which names a run xplane-run-<C2 id> (C2).
var DefaultKind = Kind{Group: "cloud.ogenki.io", Namespace: "agents", NamePrefix: "xplane-run-"}

var (
	groupRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	nsRE     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	prefixRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}-$`) // + 8 id characters stays a DNS label
)

type state struct {
	kind    Kind
	subject *regexp.Regexp
}

// current is read on every claim; atomic so a test's Configure never races a watch goroutine.
var current atomic.Pointer[state]

func init() { current.Store(newState(DefaultKind)) }

func newState(k Kind) *state {
	return &state{kind: k, subject: regexp.MustCompile("^system:serviceaccount:" +
		regexp.QuoteMeta(k.Namespace) + ":" + regexp.QuoteMeta(k.NamePrefix) + "([a-z2-7]{8})$")}
}

// WithDefaults fills each empty field from DefaultKind.
func (k Kind) WithDefaults() Kind {
	if k.Group == "" {
		k.Group = DefaultKind.Group
	}
	if k.Namespace == "" {
		k.Namespace = DefaultKind.Namespace
	}
	if k.NamePrefix == "" {
		k.NamePrefix = DefaultKind.NamePrefix
	}
	return k
}

// Validate reports every malformed field at once.
func (k Kind) Validate() error {
	var errs []error
	if !groupRE.MatchString(k.Group) {
		errs = append(errs, fmt.Errorf("group %q is not an API group (a DNS subdomain with a dot)", k.Group))
	}
	if !nsRE.MatchString(k.Namespace) {
		errs = append(errs, fmt.Errorf("namespace %q is not a DNS label", k.Namespace))
	}
	if err := ValidatePrefix(k.NamePrefix); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ValidatePrefix refuses a run-name prefix that, with 8 id characters, is not a DNS label.
func ValidatePrefix(p string) error {
	if !prefixRE.MatchString(p) {
		return fmt.Errorf("namePrefix %q must match %s", p, prefixRE)
	}
	return nil
}

// Configure sets the kind every reader uses, its empty fields defaulted. Call it once at
// startup, before a watch or a client reads a run. A refused kind leaves the current one.
func Configure(k Kind) error {
	k = k.WithDefaults()
	if err := k.Validate(); err != nil {
		return err
	}
	current.Store(newState(k))
	return nil
}

// Current is the configured kind.
func Current() Kind { return current.Load().kind }

// Namespace is where every run lives.
func Namespace() string { return Current().Namespace }

// ClaimPrefix starts every run's name: <prefix><C2 id>.
func ClaimPrefix() string { return Current().NamePrefix }

// GVK is the AgentRun's kind (C3).
func GVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: Current().Group, Version: "v1alpha1", Kind: "AgentRun"}
}

// RunSubject matches a run's ServiceAccount token subject, capturing the run id: the
// ServiceAccount is named exactly like the run, on both runtimes.
func RunSubject() *regexp.Regexp { return current.Load().subject }
