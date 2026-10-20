// SPDX-License-Identifier: Apache-2.0

// Package runctl is the run-controller (adoption 3a): it reconciles agents.ogenki.io AgentRuns
// into the pod, ServiceAccount, ConfigMaps and CiliumNetworkPolicy that cloud-native-ref's
// Crossplane composition builds, minus the gateway, octo-sts, traces, Kueue and suspend.
package runctl

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/runwatch"
)

// Config is the run-controller's file, from the chart.
type Config struct {
	Namespace        string `json:"namespace"`
	NamePrefix       string `json:"namePrefix"`
	RuntimeClassName string `json:"runtimeClassName,omitempty"`
	// AllowUnsandboxed places pods without a RuntimeClass; each run then carries condition
	// Unsandboxed, so the room can say so (3a R5).
	AllowUnsandboxed bool                `json:"allowUnsandboxed,omitempty"`
	ModelSecret      string              `json:"modelSecret"`
	GitSecret        string              `json:"gitSecret"`
	Broker           Broker              `json:"broker"`
	Images           Images              `json:"images"`
	EgressProfiles   map[string][]string `json:"egressProfiles,omitempty"`
}

// Broker is where the bridge connects, and what the CNP lets it reach.
type Broker struct {
	URL       string            `json:"url"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels"`
	CASecret  string            `json:"caSecret"`
}

// Images are the run pod's two images, pinned by the chart.
type Images struct {
	Harness string `json:"harness"`
	Bridge  string `json:"bridge"`
}

// defaultProfiles are the composition's _EGRESS_PROFILES (crossplane-configuration v0.9.15).
var defaultProfiles = map[string][]string{
	"github": {"github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com"},
	"pypi":   {"pypi.org", "files.pythonhosted.org"},
	"npm":    {"registry.npmjs.org"},
	"golang": {"proxy.golang.org", "sum.golang.org"},
	"crates": {"index.crates.io", "static.crates.io"},
}

// The broker host must be the fully qualified service name: the pod's ndots:1 resolver
// would otherwise try search-path expansions that the CNP's DNS allowlist refuses.
var brokerHostRE = regexp.MustCompile(`^[a-z0-9-]+\.[a-z0-9-]+\.svc\.[a-z0-9.-]+$`)

// LoadConfig reads, strictly decodes, defaults and validates the file at path.
func LoadConfig(path string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return c, fmt.Errorf("config: %w", err)
	}
	if err := yaml.UnmarshalStrict(raw, &c); err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	if c.EgressProfiles == nil {
		c.EgressProfiles = defaultProfiles
	}
	if err := c.validate(); err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

func (c Config) validate() error {
	var errs []error
	req := func(name, v string) {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	req("namespace", c.Namespace)
	req("modelSecret", c.ModelSecret)
	req("gitSecret", c.GitSecret)
	req("broker.namespace", c.Broker.Namespace)
	req("broker.caSecret", c.Broker.CASecret)
	req("images.harness", c.Images.Harness)
	req("images.bridge", c.Images.Bridge)
	if err := runwatch.ValidatePrefix(c.NamePrefix); err != nil {
		errs = append(errs, err)
	}
	u, err := url.Parse(c.Broker.URL)
	switch {
	case err != nil || u.Scheme != "https" || u.Hostname() == "":
		errs = append(errs, fmt.Errorf("broker.url %q must be an https URL", c.Broker.URL))
	case !brokerHostRE.MatchString(u.Hostname()):
		errs = append(errs, fmt.Errorf("broker.url host %q must be <service>.<namespace>.svc.<cluster-domain>, e.g. room-broker.agent-platform.svc.cluster.local", u.Hostname()))
	}
	if len(c.Broker.Labels) == 0 {
		errs = append(errs, errors.New("broker.labels: the CNP needs at least one to select the broker"))
	}
	if len(c.EgressProfiles["github"]) == 0 {
		errs = append(errs, errors.New("egressProfiles.github is required: every run clones from GitHub"))
	}
	return errors.Join(errs...)
}

// RunID is the run's C2 id, or "" for a name this controller does not own.
func (c Config) RunID(r *v1alpha1.AgentRun) string {
	id, ok := strings.CutPrefix(r.Name, c.NamePrefix)
	if !ok || !envelope.ValidID(id) {
		return ""
	}
	return id
}

// Invalid says why the controller refuses r, or "" when it accepts it (3a R6, R7).
func (c Config) Invalid(r *v1alpha1.AgentRun) string {
	switch {
	case c.RunID(r) == "":
		return fmt.Sprintf("the name must be namePrefix %q followed by 8 characters of [a-z2-7]", c.NamePrefix)
	case r.Namespace != c.Namespace:
		return fmt.Sprintf("runs live in namespace %q", c.Namespace)
	case r.Spec.Suspendable:
		return "spec.suspendable runs need cloud-native-ref's runtime"
	case r.Spec.DataClass == "internal":
		return "spec.dataClass internal needs the model gateway, which this runtime does not have"
	case c.RuntimeClassName == "" && !c.AllowUnsandboxed:
		return "runtimeClassName is empty and allowUnsandboxed is false: refusing to run unsandboxed"
	}
	for _, p := range r.Spec.Egress.Profiles {
		if _, ok := c.EgressProfiles[string(p)]; !ok {
			return fmt.Sprintf("spec.egress.profiles: profile %q is not configured", p)
		}
	}
	return ""
}

// FQDNs are the hosts the run may reach on 443: github, then its profiles, deduplicated.
func (c Config) FQDNs(r *v1alpha1.AgentRun) []string {
	out := slices.Clone(c.EgressProfiles["github"])
	for _, p := range r.Spec.Egress.Profiles {
		for _, h := range c.EgressProfiles[string(p)] {
			out = addUnique(out, h)
		}
	}
	return out
}

// addUnique appends h unless list already holds it.
func addUnique(list []string, h string) []string {
	if slices.Contains(list, h) {
		return list
	}
	return append(list, h)
}

// Model is the provider the harness calls directly (3a R4), from modelSecret.
type Model struct {
	BaseURL, Name, Host string
	Port                int
}

const defaultBaseURL = "https://api.anthropic.com/v1"

// ModelFrom reads modelSecret's data: api_key and model required, base_url defaulted.
func ModelFrom(data map[string][]byte) (Model, error) {
	if len(data["api_key"]) == 0 {
		return Model{}, errors.New("modelSecret has no api_key")
	}
	m := Model{BaseURL: strings.TrimSpace(string(data["base_url"])), Name: strings.TrimSpace(string(data["model"]))}
	if m.Name == "" {
		return Model{}, errors.New("modelSecret has no model")
	}
	if m.BaseURL == "" {
		m.BaseURL = defaultBaseURL
	}
	u, err := url.Parse(m.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return Model{}, fmt.Errorf("modelSecret base_url %q is not an http(s) URL", m.BaseURL)
	}
	m.Host, m.Port = u.Hostname(), 443
	if u.Scheme == "http" {
		m.Port = 80
	}
	if p := u.Port(); p != "" {
		m.Port, _ = strconv.Atoi(p)
	}
	return m, nil
}
