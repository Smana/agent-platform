// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/Smana/agent-platform/internal/envelope"
)

// RunIssuer is one allowlisted issuer and how its subject names a run. Today the
// only issuer is the cluster's (sub system:serviceaccount:agents:xplane-run-<runId>);
// a runtime minting its own workload JWTs is one more entry, not a new path (C2 r5).
type RunIssuer struct {
	Verifier   *Verifier
	SubPattern *regexp.Regexp // one capture group: the runId; must match the whole subject
}

// Runs authenticates run bridges against the allowlisted issuers.
type Runs struct{ issuers []RunIssuer }

// NewRuns allowlists issuers, tried in order. Each needs a Verifier and a
// SubPattern with exactly one capture group.
func NewRuns(issuers ...RunIssuer) (*Runs, error) {
	if len(issuers) == 0 {
		return nil, errors.New("authn: at least one run issuer is required")
	}
	for i, is := range issuers {
		if is.Verifier == nil || is.SubPattern == nil || is.SubPattern.NumSubexp() != 1 {
			return nil, fmt.Errorf("authn: run issuer %d needs a Verifier and a SubPattern with one capture group", i)
		}
	}
	return &Runs{issuers: issuers}, nil
}

// Authenticate maps a bridge's token to agent:<runId>. It does NOT check that the
// run is live: the caller does, against the AgentRun watch.
func (r *Runs) Authenticate(req *http.Request) (Principal, error) {
	raw, err := Bearer(req)
	if err != nil {
		return Principal{}, err
	}
	for _, is := range r.issuers {
		c, err := is.Verifier.Verify(req.Context(), raw, AudienceRun)
		if errors.Is(err, ErrWrongIssuer) {
			continue
		}
		if err != nil {
			return Principal{}, err
		}
		// m[0] == Subject: a pattern missing its anchors must not accept a
		// subject that merely contains a run's name.
		m := is.SubPattern.FindStringSubmatch(c.Subject)
		if len(m) != 2 || m[0] != c.Subject || !envelope.ValidID(m[1]) {
			return Principal{}, fmt.Errorf("%w: the subject names no run", ErrUnauthenticated)
		}
		return Principal{Kind: envelope.ActorAgent, ID: "agent:" + m[1], RunID: m[1], Sub: c.Subject,
			Expiry: c.ExpiresAt.Time}, nil
	}
	return Principal{}, fmt.Errorf("%w: no allowlisted issuer", ErrWrongIssuer)
}
