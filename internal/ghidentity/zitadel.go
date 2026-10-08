// SPDX-License-Identifier: Apache-2.0

package ghidentity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/httpx"
)

const (
	linksTimeout = 10 * time.Second
	maxLinksBody = 64 << 10
)

// ZitadelLinks lists a user's IdP links through ZITADEL's v2 API, authenticated with pat. Errors
// never carry the PAT: it lives only in the request header, and a transport error is reduced to
// its cause.
func ZitadelLinks(hc *http.Client, issuer, pat string) func(ctx context.Context, user string) ([]Link, error) {
	base := strings.TrimRight(issuer, "/")
	return func(ctx context.Context, user string) ([]Link, error) {
		ctx, cancel := context.WithTimeout(ctx, linksTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			base+"/v2/users/"+url.PathEscape(user)+"/links/_search", bytes.NewReader([]byte("{}")))
		if err != nil {
			return nil, errors.New("zitadel links: bad request")
		}
		req.Header.Set("Authorization", "Bearer "+pat)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return nil, fmt.Errorf("zitadel links: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("zitadel links: HTTP %d", resp.StatusCode)
		}
		raw, err := httpx.ReadBody(resp.Body, maxLinksBody)
		if err != nil {
			return nil, fmt.Errorf("zitadel links: %w", err)
		}
		var out struct {
			Result []struct {
				IDPID  string `json:"idpId"`
				UserID string `json:"userId"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, errors.New("zitadel links: the reply is not JSON")
		}
		links := make([]Link, 0, len(out.Result))
		for _, l := range out.Result {
			links = append(links, Link{IDPID: l.IDPID, UserID: l.UserID})
		}
		return links, nil
	}
}
