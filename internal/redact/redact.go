// Package redact removes secrets before anything reaches the log (SP2 §4, T8).
// It uses gitleaks' default rules; the test pins the four the design names.
package redact

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
)

type Redactor struct{ cfg config.Config }

// New parses gitleaks' default configuration once.
func New() (*Redactor, error) {
	d, err := detect.NewDetectorDefaultConfig()
	if err != nil {
		return nil, err
	}
	return &Redactor{cfg: d.Config}, nil
}

// String replaces each finding's secret with [REDACTED:<rule>]. A fresh Detector per
// call: a Detector accumulates every finding it ever made, which would grow forever
// in a long-lived broker.
func (r *Redactor) String(s string) (string, []string) {
	return r.scan(detect.NewDetector(r.cfg), s, map[string]bool{})
}

func (r *Redactor) scan(d *detect.Detector, s string, fired map[string]bool) (string, []string) {
	for _, f := range d.DetectString(s) {
		secret := f.Secret
		if secret == "" {
			secret = f.Match
		}
		if secret == "" {
			continue
		}
		s = strings.ReplaceAll(s, secret, "[REDACTED:"+f.RuleID+"]")
		fired[f.RuleID] = true
	}
	return s, keys(fired)
}

// Payload redacts every string value of a JSON document and keeps its shape.
// Each value is scanned alone: a secret split across sibling fields is not detected, so callers must not split tokens.
func (r *Redactor) Payload(raw json.RawMessage) (json.RawMessage, []string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, nil, err
	}
	d := detect.NewDetector(r.cfg)
	fired := map[string]bool{}
	v = r.walk(d, v, fired)
	out, err := json.Marshal(v)
	return out, keys(fired), err
}

func (r *Redactor) walk(d *detect.Detector, v any, fired map[string]bool) any {
	switch t := v.(type) {
	case string:
		s, _ := r.scan(d, strings.ReplaceAll(t, "\x00", ""), fired) // jsonb refuses NUL (review I6)
		return s
	case []any:
		for i := range t {
			t[i] = r.walk(d, t[i], fired)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[strings.ReplaceAll(k, "\x00", "")] = r.walk(d, e, fired)
		}
		return out
	default:
		return v
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
