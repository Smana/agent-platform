// SPDX-License-Identifier: Apache-2.0

// Package redact removes secrets before anything reaches the log (SP2 §4, T8).
// It uses gitleaks' default rules; the test pins the four the design names.
package redact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
)

// ErrKeyCollision reports two object keys that are one once NULs and secrets
// are removed. Keeping either would drop the other's value, and which one
// depends on map order, so the payload is refused. The error never quotes a key.
var ErrKeyCollision = errors.New("redact: two object keys are the same once redacted")

// Redactor applies gitleaks' default rules to text and JSON payloads.
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
	fired := map[string]bool{}
	return r.scan(detect.NewDetector(r.cfg), s, fired), keys(fired)
}

func (r *Redactor) scan(d *detect.Detector, s string, fired map[string]bool) string {
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
	return s
}

// Payload redacts every string of a JSON document, object keys included, and
// keeps its shape. Each string is scanned alone: a secret split across sibling
// fields is not detected, so callers must not split tokens.
//
// It honours ctx between strings, and an ended ctx fails the whole payload: it
// is never returned partly scanned. One string's scan is not interruptible:
// gitleaks' ctx-aware entry point takes its deprecated Fragment type, and the
// caller's body limit bounds a string anyway.
func (r *Redactor) Payload(ctx context.Context, raw json.RawMessage) (json.RawMessage, []string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, nil, err
	}
	d := detect.NewDetector(r.cfg)
	fired := map[string]bool{}
	v, err := r.walk(ctx, d, v, fired)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("redact: %w", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return out, keys(fired), nil
}

func (r *Redactor) walk(ctx context.Context, d *detect.Detector, v any, fired map[string]bool) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch t := v.(type) {
	case string:
		return r.scan(d, strings.ReplaceAll(t, "\x00", ""), fired), nil // jsonb refuses NUL (review I6)
	case []any:
		for i := range t {
			e, err := r.walk(ctx, d, t[i], fired)
			if err != nil {
				return nil, err
			}
			t[i] = e
		}
		return t, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			// A key is text like any other: an env dump puts secrets there.
			key := r.scan(d, strings.ReplaceAll(k, "\x00", ""), fired)
			if _, merged := out[key]; merged {
				return nil, ErrKeyCollision
			}
			val, err := r.walk(ctx, d, e, fired)
			if err != nil {
				return nil, err
			}
			out[key] = val
		}
		return out, nil
	default:
		return v, nil
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
