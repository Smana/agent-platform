// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"bytes"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// get runs one request through c and closes the response; the tests assert on
// the error and on what the servers saw.
func get(t *testing.T, c *http.Client, url string, header http.Header) error {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func TestNewSetsTheTimeout(t *testing.T) {
	cases := map[string]struct {
		in, want time.Duration
	}{
		"explicit":            {3 * time.Second, 3 * time.Second},
		"zero is default":     {0, DefaultTimeout},
		"negative is default": {-time.Second, DefaultTimeout},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := New(c.in, nil).Timeout; got != c.want {
				t.Fatalf("Timeout = %v, want %v", got, c.want)
			}
		})
	}
}

func TestTheTimeoutFires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	err := get(t, New(50*time.Millisecond, nil), srv.URL, nil)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("want a timeout, got %v", err)
	}
}

func TestCloudMetadataAddressesAreRefused(t *testing.T) {
	for _, url := range []string{
		"http://169.254.169.254/latest/meta-data/",         // IMDS, GCE metadata server
		"http://169.254.170.23/v1/credentials",             // EKS Pod Identity agent
		"http://169.254.170.2/v2/credentials",              // ECS task role
		"http://[fd00:ec2::254]/latest/meta-data/",         // IMDS over IPv6
		"http://[fd00:ec2::23]/v1/credentials",             // Pod Identity over IPv6
		"http://[fd20:ce::254]/computeMetadata/v1/",        // GCE metadata over IPv6
		"http://[::ffff:169.254.169.254]/latest/api/token", // IPv4-mapped
		"http://[fe80::1]/",                                // any link-local
	} {
		t.Run(url, func(t *testing.T) {
			err := get(t, New(time.Second, nil), url, nil)
			if !errors.Is(err, ErrForbiddenAddress) {
				t.Fatalf("want ErrForbiddenAddress, got %v", err)
			}
		})
	}
}

func TestRedirects(t *testing.T) {
	// seen records the credential headers the final hop received.
	var seen http.Header
	var originURL string
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/back" {
			http.Redirect(w, r, originURL+"/done", http.StatusFound)
			return
		}
		seen = r.Header.Clone()
	}))
	defer final.Close()
	// Another port on the same name: Go forwards credentials there, we do not.
	otherHost := final.URL

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/same-host":
			http.Redirect(w, r, "/done", http.StatusFound)
		case "/other-host":
			http.Redirect(w, r, otherHost+"/done", http.StatusFound)
		case "/away-and-back":
			http.Redirect(w, r, otherHost+"/back", http.StatusFound)
		case "/metadata":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		case "/done":
			seen = r.Header.Clone()
		}
	}))
	defer origin.Close()
	originURL = origin.URL

	creds := http.Header{
		"Authorization":       {"Bearer secret"},
		"Proxy-Authorization": {"Basic secret"},
		"Cookie":              {"session=secret"},
	}
	cases := map[string]struct {
		path      string
		wantErr   error
		wantCreds bool
	}{
		"the chain is capped":                          {path: "/loop", wantErr: ErrTooManyRedirects},
		"a metadata target is refused":                 {path: "/metadata", wantErr: ErrForbiddenAddress},
		"credentials survive a same-host hop":          {path: "/same-host", wantCreds: true},
		"credentials are stripped on a host change":    {path: "/other-host", wantCreds: false},
		"credentials stay stripped back on the origin": {path: "/away-and-back", wantCreds: false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			seen = nil
			err := get(t, New(5*time.Second, nil), origin.URL+c.path, creds)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("want %v, got %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if seen == nil {
				t.Fatal("the final hop was not reached")
			}
			for h := range creds {
				if got := seen.Get(h) != ""; got != c.wantCreds {
					t.Errorf("%s forwarded = %v, want %v", h, got, c.wantCreds)
				}
			}
		})
	}
}

func TestAnHTTPSDowngradeIsRefused(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer plain.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer tlsSrv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(tlsSrv.Certificate())

	err := get(t, New(5*time.Second, roots), tlsSrv.URL, nil)
	if !errors.Is(err, ErrInsecureRedirect) {
		t.Fatalf("want ErrInsecureRedirect, got %v", err)
	}
}

func TestRootsAreTrusted(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	if err := get(t, New(5*time.Second, nil), srv.URL, nil); err == nil {
		t.Fatal("a server outside the system pool must be refused without its CA")
	}
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	if err := get(t, New(5*time.Second, roots), srv.URL, nil); err != nil {
		t.Fatalf("with its CA: %v", err)
	}
}

func TestReadBody(t *testing.T) {
	cases := map[string]struct {
		size    int
		wantErr error
	}{
		"under the limit":    {size: 9},
		"exactly the limit":  {size: 10},
		"one byte over":      {size: 11, wantErr: ErrBodyTooLarge},
		"far over the limit": {size: 1 << 20, wantErr: ErrBodyTooLarge},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ReadBody(bytes.NewReader(make([]byte, c.size)), 10)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if c.wantErr == nil && len(got) != c.size {
				t.Fatalf("read %d bytes, want %d", len(got), c.size)
			}
		})
	}
}

// Go keeps credentials from example.com to a subdomain of it; a subdomain is
// another service here. No network: checkRedirect runs on the request Go built.
func TestASubdomainIsAnotherHost(t *testing.T) {
	first := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/jwks", nil)
	next := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://keys.example.com/jwks", nil)
	next.Header.Set("Authorization", "Bearer secret")
	if err := checkRedirect(next, []*http.Request{first}); err != nil {
		t.Fatal(err)
	}
	if next.Header.Get("Authorization") != "" {
		t.Fatal("Authorization was forwarded to a subdomain")
	}
}
