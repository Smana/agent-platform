// SPDX-License-Identifier: Apache-2.0

// Package config is the broker's one config file (a ConfigMap, Flux-substituted).
// Load decodes it strictly, applies the defaults and validates it, so a typo or a
// bad pattern fails the rollout rather than the first request.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// The :8443 key pair's defaults: cert-manager's Certificate room-broker-tls (GP-18).
const (
	DefaultCertFile = "/etc/room-broker/tls/tls.crt"
	DefaultKeyFile  = "/etc/room-broker/tls/tls.key"
)

// IssuerConfig is one allowlisted token issuer. SubPattern, for a run issuer
// only, maps a subject to its runId with exactly one capture group.
type IssuerConfig struct {
	Issuer     string `json:"issuer"`
	JWKSURL    string `json:"jwksURL"`
	SubPattern string `json:"subPattern,omitempty"`
}

// HumanConfig is the viewers' identity provider (phase 2). The client and project
// ids are read from mounted files at use, never literals (Ruling AS-a): the IdP
// mints them, anew on every gcp-0 build, and a rotation needs no restart.
type HumanConfig struct {
	Issuer              string       `json:"issuer"`
	JWKSURL             string       `json:"jwksURL"`
	ClientIDFile        string       `json:"clientIDFile"`                  // the rooms-proxy client id, from agents-secrets
	RoomctlClientIDFile string       `json:"roomctlClientIDFile,omitempty"` // phase 6
	ProjectIDFile       string       `json:"projectIDFile"`                 // a human token's aud must hold it (Ruling AS)
	Origin              string       `json:"origin"`
	Groups              GroupsConfig `json:"groups"`
	// Access turns on D7: a member sees a room only if they can read its repository
	// on GitHub. Unset, rooms are visible to admins only, never to every member.
	Access *AccessConfig `json:"access,omitempty"`
}

// AccessConfig is D7's GitHub check. ReaderFile is the ZITADEL link reader's mounted
// secret, JSON {"pat", "tokenId", "githubIdpId"}, read at use: ZITADEL at the human
// issuer resolves a member to their linked GitHub id. TTL caches each answer.
type AccessConfig struct {
	ReaderFile string   `json:"readerFile"`
	TTL        Duration `json:"ttl,omitempty"`
}

// MaxAccessTTL is the longest a GitHub access answer is trusted, and the default: a
// revoked read must lapse within it (D7).
const MaxAccessTTL = 5 * time.Minute

// Duration is a time.Duration written as a string such as "90s".
type Duration struct{ time.Duration }

// UnmarshalJSON parses a Go duration string; a bare number is refused, since its unit is a guess.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("a duration is a string such as \"90s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

// GroupsConfig names the IdP's two agent groups, which feed policy.Groups. They
// are literals: names we choose, not ids the IdP mints.
type GroupsConfig struct {
	Admin  string `json:"admin"`
	Member string `json:"member"`
}

// TLSConfig names the :8443 key pair, re-read when it changes (GP-18).
type TLSConfig struct {
	CertFile string `json:"certFile,omitempty"`
	KeyFile  string `json:"keyFile,omitempty"`
}

// Config is the broker's configuration.
type Config struct {
	PublicURL        string            `json:"publicURL"`
	RunIssuers       []IssuerConfig    `json:"runIssuers"`
	SystemIssuer     IssuerConfig      `json:"systemIssuer"`
	SystemPrincipals map[string]string `json:"systemPrincipals"` // token sub -> system:<component>
	Human            HumanConfig       `json:"human"`
	FactoryURL       string            `json:"factoryURL,omitempty"`
	TLS              TLSConfig         `json:"tls"`
}

// Load reads, strictly decodes, defaults and validates the file at path.
func Load(path string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return c, fmt.Errorf("config: %w", err)
	}
	if err := yaml.UnmarshalStrict(raw, &c); err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

// ApplyDefaults fills what the file may leave out.
func (c *Config) ApplyDefaults() {
	if c.TLS.CertFile == "" {
		c.TLS.CertFile = DefaultCertFile
	}
	if c.TLS.KeyFile == "" {
		c.TLS.KeyFile = DefaultKeyFile
	}
	if a := c.Human.Access; a != nil && a.TTL.Duration == 0 {
		a.TTL.Duration = MaxAccessTTL
	}
}

// Validate reports every problem at once. The run issuers' patterns compile here,
// so the broker's own regexp.MustCompile cannot panic.
func (c *Config) Validate() error {
	var errs []error
	if len(c.RunIssuers) == 0 {
		errs = append(errs, errors.New("runIssuers: at least one is required"))
	}
	for i, is := range c.RunIssuers {
		errs = append(errs, issuer(fmt.Sprintf("runIssuers[%d]", i), is)...)
		re, err := regexp.Compile(is.SubPattern)
		if err != nil || re.NumSubexp() != 1 {
			errs = append(errs, fmt.Errorf("runIssuers[%d] %s: subPattern must compile with one capture group (the runId)", i, is.Issuer))
		}
	}
	errs = append(errs, issuer("systemIssuer", c.SystemIssuer)...)
	for sub, id := range c.SystemPrincipals {
		if !strings.HasPrefix(id, "system:") || id == "system:" {
			errs = append(errs, fmt.Errorf("systemPrincipals[%s]: %q must be system:<component>", sub, id))
		}
	}
	errs = append(errs, c.Human.validate()...)
	return errors.Join(errs...)
}

func (h HumanConfig) validate() []error {
	errs := issuer("human", IssuerConfig{Issuer: h.Issuer, JWKSURL: h.JWKSURL})
	if h.ClientIDFile == "" {
		errs = append(errs, errors.New("human.clientIDFile: is required"))
	}
	if h.ProjectIDFile == "" {
		errs = append(errs, errors.New("human.projectIDFile: is required"))
	}
	// Compared verbatim with the Origin header, which is scheme://host[:port].
	if u, err := url.Parse(h.Origin); err != nil || u.Scheme != "https" || u.Host == "" || u.Scheme+"://"+u.Host != h.Origin {
		errs = append(errs, errors.New("human.origin: must be https://host[:port], as a browser sends it"))
	}
	if h.Groups.Admin == "" {
		errs = append(errs, errors.New("human.groups.admin: is required"))
	}
	if h.Groups.Member == "" {
		errs = append(errs, errors.New("human.groups.member: is required"))
	}
	if h.Groups.Admin != "" && h.Groups.Admin == h.Groups.Member {
		errs = append(errs, errors.New("human.groups: admin and member must differ"))
	}
	if a := h.Access; a != nil {
		if a.ReaderFile == "" {
			errs = append(errs, errors.New("human.access.readerFile: is required"))
		}
		if a.TTL.Duration <= 0 || a.TTL.Duration > MaxAccessTTL {
			errs = append(errs, fmt.Errorf("human.access.ttl: must be above 0 and at most %s (D7)", MaxAccessTTL))
		}
		// The reader's PAT is sent to the issuer: never in clear, never with userinfo.
		if u, err := url.Parse(h.Issuer); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			errs = append(errs, errors.New("human.issuer: must be https://host when human.access is set"))
		}
	}
	return errs
}

func issuer(field string, is IssuerConfig) []error {
	var errs []error
	if is.Issuer == "" {
		errs = append(errs, fmt.Errorf("%s: issuer is required", field))
	}
	if u, err := url.Parse(is.JWKSURL); err != nil || u.Scheme != "https" || u.Host == "" {
		errs = append(errs, fmt.Errorf("%s: jwksURL must be an absolute https:// URL", field))
	}
	return errs
}
