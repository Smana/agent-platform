// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// R20: an internet subscriber may never resolve to an internal address.
func TestGuard(t *testing.T) {
	for _, a := range []string{
		"127.0.0.1", "127.1.2.3", "::1", // loopback
		"169.254.169.254", "169.254.0.1", "fe80::1", // link-local, metadata
		"10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", "fc00::1", "fd12::1", // private
		"100.64.0.1", "100.127.255.255", // CGNAT
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "::ffff:100.64.0.1", // v4-mapped
		"0.0.0.0", "::", // unspecified
		"224.0.0.1", "239.255.255.255", "ff02::1", // multicast
		"0.1.2.3", "0.255.255.255", // 0.0.0.0/8
		"192.0.0.1", "192.0.0.170", // 192.0.0.0/24
		"198.18.0.1", "198.19.255.255", // 198.18.0.0/15
		"240.0.0.1", "255.255.255.255", // 240.0.0.0/4
		"64:ff9b::808:808", "64:ff9b::7f00:1", "64:ff9b:1::1", "64:ff9b:1:ffff::1", // NAT64
		"2002:7f00:1::1", "2002:808:808::1", // 6to4
	} {
		if err := Guard(netip.MustParseAddr(a)); err == nil || !strings.Contains(err.Error(), "webhook: refused address") {
			t.Errorf("Guard(%s) = %v, want refused", a, err)
		}
	}
	for _, a := range []string{"1.1.1.1", "8.8.8.8", "100.63.255.255", "100.128.0.1", "2606:4700::1111", "::ffff:1.1.1.1", "2001:4860:4860::8888", "198.17.255.255", "198.20.0.1", "192.0.1.1"} {
		if err := Guard(netip.MustParseAddr(a)); err != nil {
			t.Errorf("Guard(%s) = %v, want allowed", a, err)
		}
	}
}

// The check is on the dialled address: an IP literal and a name that resolves to loopback are
// both refused at connect, which is also what a DNS rebind looks like to the dialer.
func TestSafeClientRefusesAtDial(t *testing.T) {
	var hit atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Add(1) }))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]
	c := SafeClient(2 * time.Second)
	for _, u := range []string{srv.URL, "http://localhost" + port, "http://[::ffff:127.0.0.1]" + port, "http://[::1]" + port, "http://0.0.0.0" + port} {
		resp, err := c.Post(u, "application/json", strings.NewReader("{}")) //nolint:noctx // a test of the dialer
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("%s: dialled", u)
		}
		if !strings.Contains(err.Error(), "webhook: refused address") && !strings.Contains(err.Error(), "refused") {
			t.Fatalf("%s: %v", u, err)
		}
	}
	if hit.Load() != 0 {
		t.Fatal("the server was reached")
	}
}

// No redirect is followed, so none reaches a refused address; the transport is swapped for a
// plain one here only so the first hop can be reached on loopback.
func TestSafeClientNeverFollowsRedirects(t *testing.T) {
	var target atomic.Int32
	dest := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { target.Add(1) }))
	defer dest.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL, http.StatusTemporaryRedirect)
	}))
	defer src.Close()
	c := *SafeClient(2 * time.Second)
	if c.CheckRedirect == nil {
		t.Fatal("no CheckRedirect")
	}
	c.Transport = http.DefaultTransport
	resp, err := c.Post(src.URL, "application/json", strings.NewReader("{}")) //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || target.Load() != 0 {
		t.Fatalf("status %d, target hits %d: the redirect was followed", resp.StatusCode, target.Load())
	}
}

func TestSafeClientIgnoresProxyEnv(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	tr, ok := SafeClient(time.Second).Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatal("the transport may use a proxy, which dials past Guard")
	}
}
