// SPDX-License-Identifier: Apache-2.0

// Package httpx is the one audited egress client. Every outbound HTTP call goes
// through New: it has an explicit timeout, caps redirects, refuses an https→http
// downgrade, strips credential headers once a chain leaves its first host, and
// refuses to dial a cloud metadata address. ReadBody bounds what a response may
// put in memory.
package httpx

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// DefaultTimeout bounds a whole exchange when New is given no positive timeout.
const DefaultTimeout = 10 * time.Second

// maxRedirects is explicit because installing a CheckRedirect drops Go's own cap.
const maxRedirects = 5

var (
	// ErrForbiddenAddress reports a dial to a cloud metadata address.
	ErrForbiddenAddress = errors.New("egress to a cloud metadata address")
	// ErrTooManyRedirects reports a redirect chain longer than the cap.
	ErrTooManyRedirects = errors.New("too many redirects")
	// ErrInsecureRedirect reports an https→http redirect.
	ErrInsecureRedirect = errors.New("redirect from https to http")
	// ErrBodyTooLarge reports a response body over the caller's limit.
	ErrBodyTooLarge = errors.New("response body too large")
)

// credentialHeaders are dropped once a redirect chain leaves its first host. Go
// drops Authorization and Cookie itself, but keeps them for a subdomain and never
// drops Proxy-Authorization.
var credentialHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie"}

// metadataIPv6 are the IPv6 metadata endpoints outside fe80::/10: AWS IMDS,
// the EKS Pod Identity agent and the GCE metadata server.
var metadataIPv6 = []netip.Addr{
	netip.MustParseAddr("fd00:ec2::254"),
	netip.MustParseAddr("fd00:ec2::23"),
	netip.MustParseAddr("fd20:ce::254"),
}

// New returns the egress client. A timeout ≤ 0 means DefaultTimeout. roots, when
// not nil, replaces the system pool (a private CA such as the broker's).
//
// The guard sits at dial time, not in CheckRedirect, so it covers the first
// request, every redirect and a DNS name that resolves to a metadata address. No
// proxy is configured: a proxy would make the dialed address the proxy's.
func New(timeout time.Duration, roots *x509.CertPool) *http.Client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second, Control: refuseMetadata}
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: checkRedirect,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: timeout,
			ExpectContinueTimeout: time.Second,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		},
	}
}

// refuseMetadata runs after DNS resolution and before connect, on the address
// actually dialed. All of 169.254.0.0/16 and fe80::/10 is refused: the metadata
// servers (169.254.169.254), the EKS Pod Identity agent (169.254.170.23) and the
// ECS credential endpoint (169.254.170.2) all live there, and nothing this
// service calls does.
func refuseMetadata(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("dial %q: %w", address, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("dial %q: %w", address, err)
	}
	ip = ip.Unmap()
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("%w: %s", ErrForbiddenAddress, ip)
	}
	for _, m := range metadataIPv6 {
		if ip == m {
			return fmt.Errorf("%w: %s", ErrForbiddenAddress, ip)
		}
	}
	return nil
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("%w: %d", ErrTooManyRedirects, maxRedirects)
	}
	prev := via[len(via)-1]
	if prev.URL.Scheme == "https" && req.URL.Scheme != "https" {
		return ErrInsecureRedirect
	}
	// Against the first host, over the whole chain: A→B→A must not regain what
	// the hop to B dropped. Host includes the port, so another port on the same
	// name is another service.
	for _, hop := range append(via[1:], req) {
		if !strings.EqualFold(hop.URL.Host, via[0].URL.Host) {
			for _, h := range credentialHeaders {
				req.Header.Del(h)
			}
			break
		}
	}
	return nil
}

// ReadBody reads r to its end, or fails with ErrBodyTooLarge once it passes limit
// bytes. It never returns a truncated body as if it were whole.
func ReadBody(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: over %d bytes", ErrBodyTooLarge, limit)
	}
	return b, nil
}
