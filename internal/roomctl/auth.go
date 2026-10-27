// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"golang.org/x/oauth2"
)

func oauthConfig(c Config) *oauth2.Config {
	return &oauth2.Config{ClientID: c.ClientID,
		// ZITADEL puts the project id in aud only when asked (ruling AS), and
		// offline_access brings the refresh token.
		Scopes: []string{"openid", "profile", "email", "offline_access", "urn:zitadel:iam:org:project:id:" + c.ProjectID + ":aud"},
		Endpoint: oauth2.Endpoint{DeviceAuthURL: c.Issuer + "/oauth/v2/device_authorization", TokenURL: c.Issuer + "/oauth/v2/token",
			AuthStyle: oauth2.AuthStyleInParams}} // a native app: client_id in the form, no secret
}

// Login runs the OAuth device authorization grant: no browser on this machine
// needs a redirect, and no client secret exists to leak. hc carries every call.
func Login(ctx context.Context, hc *http.Client, c Config, prompt io.Writer) (*oauth2.Token, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, hc)
	conf := oauthConfig(c)
	da, err := conf.DeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("device authorization: %w%s", err, staleClientHint(err))
	}
	if _, err := fmt.Fprintf(prompt, "Open %s and enter the code %s\n", da.VerificationURI, da.UserCode); err != nil {
		return nil, err
	}
	tok, err := conf.DeviceAccessToken(ctx, da)
	if err != nil {
		return nil, fmt.Errorf("device token: %w%s", err, staleClientHint(err))
	}
	return tok, nil
}

// staleClientHint names the fix of invalid_client: after a platform rebuild the stored client id
// is no longer one, and ZITADEL's own sentence ("no active client not found") says nothing of it.
func staleClientHint(err error) string {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.ErrorCode == "invalid_client" {
		return "\nhint: the stored client id is stale (the platform was rebuilt?): run roomctl configure --from https://rooms.<your private domain>, then roomctl login"
	}
	return ""
}

// Token returns a valid access token from the token file at path, refreshing
// and saving it when it has expired.
func Token(ctx context.Context, hc *http.Client, c Config, path string) (string, error) {
	var tok oauth2.Token
	if err := LoadJSON(path, &tok); err != nil {
		return "", fmt.Errorf("not logged in (%w): run roomctl login", err)
	}
	fresh, err := oauthConfig(c).TokenSource(context.WithValue(ctx, oauth2.HTTPClient, hc), &tok).Token()
	if err != nil {
		return "", fmt.Errorf("refresh the token (%w): run roomctl login", err)
	}
	if fresh.AccessToken != tok.AccessToken {
		if err := SaveJSON(path, fresh); err != nil {
			return "", fmt.Errorf("save the refreshed token: %w", err)
		}
	}
	return fresh.AccessToken, nil
}

// SharedToken is Token for one process's concurrent callers, roomctl mcp's requests: one refresh
// at a time, so a refresh token the issuer rotates is never spent twice. Each call reads the file
// again, so a roomctl login in another terminal reaches a running relay without a restart.
func SharedToken(hc *http.Client, c Config, path string) func(context.Context) (string, error) {
	var mu sync.Mutex
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return Token(ctx, hc, c, path)
	}
}
