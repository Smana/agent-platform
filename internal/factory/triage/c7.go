// SPDX-License-Identifier: Apache-2.0

package triage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// C7Request and C7Response are the programme's C7 contract, verbatim.
type C7Request struct {
	Text      string `json:"text"`
	Ref       string `json:"ref"`
	DataClass string `json:"dataClass"`
}

// C7Shadow is a classifier consulted in shadow: recorded, never acted on.
type C7Shadow struct {
	Classifier string  `json:"classifier"`
	Tier       string  `json:"tier"`
	Confidence float64 `json:"confidence"`
}

// C7Response is the classifier's verdict: the tier, how sure it is, and what it fell back to.
type C7Response struct {
	Tier       string     `json:"tier"`
	Confidence float64    `json:"confidence"`
	Classifier string     `json:"classifier"`
	Fallback   string     `json:"fallback"`
	Shadow     []C7Shadow `json:"shadow"`
}

// Classifier is C7: one call per task, and one that never blocks (an error falls back to static).
type Classifier interface {
	Classify(ctx context.Context, r C7Request) (C7Response, error)
}

// HTTPClassifier calls C7's endpoint over HTTP (R24). HC nil uses a 3 s client: C7's own
// deadline is 2 s, so the caller waits, but never forever.
type HTTPClassifier struct {
	URL string
	HC  *http.Client
}

// Classify posts the request and decodes the verdict; a non-200 is an error, which the caller
// falls back on.
func (h HTTPClassifier) Classify(ctx context.Context, r C7Request) (C7Response, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return C7Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(b))
	if err != nil {
		return C7Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := h.HC
	if hc == nil {
		hc = &http.Client{Timeout: 3 * time.Second} // C7's own deadline is 2 s
	}
	resp, err := hc.Do(req)
	if err != nil {
		return C7Response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return C7Response{}, fmt.Errorf("classifier: %s", resp.Status)
	}
	var out C7Response
	return out, json.NewDecoder(resp.Body).Decode(&out)
}
