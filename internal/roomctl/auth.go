// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"context"
	"fmt"
	"io"
	"net/http"

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
		return nil, fmt.Errorf("device authorization: %w", err)
	}
	if _, err := fmt.Fprintf(prompt, "Open %s and enter the code %s\n", da.VerificationURI, da.UserCode); err != nil {
		return nil, err
	}
	tok, err := conf.DeviceAccessToken(ctx, da)
	if err != nil {
		return nil, fmt.Errorf("device token: %w", err)
	}
	return tok, nil
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
