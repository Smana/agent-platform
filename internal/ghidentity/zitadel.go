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

	"github.com/Smana/maku/internal/httpx"
)

const (
	linksTimeout = 10 * time.Second
	maxLinksBody = 64 << 10
)

// zitadelDo sends req with pat and returns the reply's bounded body, or its status when the
// status is not 2xx. Errors never carry the PAT: it lives only in the request header, and a
// transport error is reduced to its cause.
func zitadelDo(hc *http.Client, req *http.Request, pat string) (int, []byte, error) {
	req.Header.Set("Authorization", "Bearer "+pat)
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, nil, nil
	}
	raw, err := httpx.ReadBody(resp.Body, maxLinksBody)
	return resp.StatusCode, raw, err
}

// ZitadelActive reads whether a user is active through ZITADEL's v2 API (GET /v2/users/{id}),
// authenticated with pat: only USER_STATE_ACTIVE is, and a user ZITADEL no longer finds (404) is
// deleted, so not active. Any other failure is an error.
func ZitadelActive(hc *http.Client, issuer, pat string) func(ctx context.Context, user string) (bool, error) {
	base := strings.TrimRight(issuer, "/")
	return func(ctx context.Context, user string) (bool, error) {
		ctx, cancel := context.WithTimeout(ctx, linksTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v2/users/"+url.PathEscape(user), nil)
		if err != nil {
			return false, errors.New("zitadel user: bad request")
		}
		code, raw, err := zitadelDo(hc, req, pat)
		switch {
		case err != nil:
			return false, fmt.Errorf("zitadel user: %w", err)
		case code == http.StatusNotFound:
			return false, nil
		case code/100 != 2:
			return false, fmt.Errorf("zitadel user: HTTP %d", code)
		}
		var out struct {
			User struct {
				State string `json:"state"`
			} `json:"user"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return false, errors.New("zitadel user: the reply is not JSON")
		}
		return out.User.State == "USER_STATE_ACTIVE", nil
	}
}

// ZitadelLinks lists a user's IdP links through ZITADEL's v2 API, authenticated with pat.
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
		req.Header.Set("Content-Type", "application/json")
		code, raw, err := zitadelDo(hc, req, pat)
		switch {
		case err != nil:
			return nil, fmt.Errorf("zitadel links: %w", err)
		case code/100 != 2:
			return nil, fmt.Errorf("zitadel links: HTTP %d", code)
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
