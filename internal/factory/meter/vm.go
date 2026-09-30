// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

	"github.com/Smana/agent-platform/internal/httpx"
)

// maxVMReply bounds a query reply: one series per live or recent run, a few hundred bytes each.
const maxVMReply = 4 << 20

// maxReading is the largest count taken as a reading: far above any run's cap (5 M, C3), and
// exact in a float64, so int64 conversion never overflows.
const maxReading = 1 << 53

// runSA maps agent-router's ar_agent label (the token's sub, C5) to the run id.
var runSA = regexp.MustCompile(`^system:serviceaccount:agents:xplane-run-([a-z2-7]{8})$`)

// VM runs the meter's one instant query against VictoriaMetrics (R12): the expression of SP4
// PR 2's agent_router:run_tokens:total over the raw series, until that rule exists.
type VM struct {
	base  *url.URL
	query string
	hc    *http.Client
}

// NewVM is the source at rawURL (meter.url): http or https with a host, since vmsingle serves
// plain http in the cluster. hc comes from httpx.New.
func NewVM(rawURL, query string, hc *http.Client) (*VM, error) {
	u, err := url.Parse(rawURL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "":
		return nil, errors.New("meter: meter.url must be http(s)://<host>[/path], without credentials or query")
	case query == "":
		return nil, errors.New("meter: meter.query is required")
	case hc == nil:
		return nil, errors.New("meter: an HTTP client from httpx.New is required")
	}
	return &VM{base: u, query: query, hc: hc}, nil
}

// RunTokens is each run's raw token counter, by run id. A series that is not a run's, or whose
// value is not a finite, non-negative count, is no reading.
func (v *VM) RunTokens(ctx context.Context) (map[string]int64, error) {
	target := v.base.JoinPath("api/v1/query")
	target.RawQuery = url.Values{"query": {v.query}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("meter: request: %w", err)
	}
	resp, err := v.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("meter: query: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := httpx.ReadBody(resp.Body, maxVMReply)
	if err != nil {
		return nil, fmt.Errorf("meter: query: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("meter: query: status %d", resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, errors.New("meter: query: the reply is not JSON")
	}
	if body.Status != "success" || body.Data.ResultType != "vector" {
		return nil, fmt.Errorf("meter: query: status %q, result %q: want a success and a vector", body.Status, body.Data.ResultType)
	}
	out := map[string]int64{}
	for _, s := range body.Data.Result {
		m := runSA.FindStringSubmatch(s.Metric["ar_agent"])
		if m == nil {
			continue
		}
		str, _ := s.Value[1].(string)
		f, err := strconv.ParseFloat(str, 64)
		if err != nil || math.IsNaN(f) || f < 0 || f > maxReading {
			continue
		}
		out[m[1]] += int64(f)
	}
	return out, nil
}
