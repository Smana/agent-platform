// SPDX-License-Identifier: Apache-2.0

package ghidentity_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/ghidentity"
)

const pat = "zitadel-pat-s3cret"

func TestZitadelLinksSearchesTheUsersLinks(t *testing.T) {
	var path, auth, body, method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth, method = r.URL.Path, r.Header.Get("Authorization"), r.Method
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{"result":[{"idpId":"gh-idp","userId":"583231","userName":"stale-name"},` +
			`{"idpId":"google","userId":"x"}],"details":{"totalResult":"2"}}`))
	}))
	defer srv.Close()
	links, err := ghidentity.ZitadelLinks(srv.Client(), srv.URL+"/", pat)(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/v2/users/u1/links/_search" || auth != "Bearer "+pat || strings.TrimSpace(body) != "{}" {
		t.Fatalf("request: %s %s %q %q", method, path, auth, body)
	}
	if len(links) != 2 || links[0] != (ghidentity.Link{IdPID: "gh-idp", UserID: "583231"}) || links[1].IdPID != "google" {
		t.Fatalf("links: %+v", links)
	}
}

func TestZitadelLinksRefusesANon2xxWithoutLeakingThePAT(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad token "+r.Header.Get("Authorization"), http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := ghidentity.ZitadelLinks(srv.Client(), srv.URL, pat)(context.Background(), "u1")
	if err == nil || strings.Contains(err.Error(), pat) {
		t.Fatalf("want an error without the PAT, got %v", err)
	}
}

func TestZitadelLinksDoesNotLeakThePATOnATransportError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := ghidentity.ZitadelLinks(srv.Client(), url, pat)(context.Background(), "u1")
	if err == nil || strings.Contains(err.Error(), pat) {
		t.Fatalf("want an error without the PAT, got %v", err)
	}
}

func TestZitadelLinksBoundsTheReplyAndEscapesTheUser(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"result":[],"pad":"` + strings.Repeat("a", 70<<10) + `"}`))
	}))
	defer srv.Close()
	if _, err := ghidentity.ZitadelLinks(srv.Client(), srv.URL, pat)(context.Background(), "a/../b"); err == nil {
		t.Fatal("an oversized reply must be an error")
	}
	if raw != "/v2/users/a%2F..%2Fb/links/_search" {
		t.Fatalf("user id not escaped: %q", raw)
	}
}
