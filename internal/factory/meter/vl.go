// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Throttled names the runs a gateway answered with a rate-limit 429 lately; VL implements it
// against VictoriaLogs. A lookup failure costs only the fleet mapping for that tick: budget-run
// and budget-principal stand without it.
type Throttled interface {
	RecentlyThrottled(ctx context.Context) (map[string]bool, error)
}

// VL asks VictoriaLogs which runs agent-router answered with a rate-limit 429 lately (R13).
// The query groups by the verified identity header (x_ar_agent), never a client-supplied one.
type VL struct {
	URL, Query string
	// HC is the meter's egress: one from httpx.New. nil is an error at use, never a
	// hand-rolled client: the audited bounds (timeout, redirect cap) are the point.
	HC *http.Client
}

// RecentlyThrottled is one LogsQL query: run id → throttled. Rows without a run's verified
// identity are not that run's, and are skipped.
func (v VL) RecentlyThrottled(ctx context.Context) (map[string]bool, error) {
	if v.HC == nil {
		return nil, errors.New("meter: an HTTP client from httpx.New is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		v.URL+"/select/logsql/query?query="+url.QueryEscape(v.Query), nil)
	if err != nil {
		return nil, fmt.Errorf("meter: request: %w", err)
	}
	resp, err := v.HC.Do(req)
	if err != nil {
		return nil, fmt.Errorf("meter: 429 query: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("victorialogs: %s", resp.Status)
	}
	out := map[string]bool{}
	sc := bufio.NewScanner(resp.Body) // the stream is already bounded: the query is a stats one
	for sc.Scan() {                   // one JSON object per line
		var row map[string]string
		if json.Unmarshal(sc.Bytes(), &row) != nil {
			continue
		}
		if m := runSA.FindStringSubmatch(row["log.x_ar_agent"]); m != nil {
			out[m[1]] = true
		}
	}
	return out, sc.Err()
}
