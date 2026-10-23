// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Guard refuses an address an internet subscriber may never resolve to (R20): loopback, private,
// link-local (cloud metadata included), CGNAT (the tailnet), unique-local and unspecified.
func Guard(a netip.Addr) error {
	a = a.Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsUnspecified() ||
		a.IsMulticast() || cgnat.Contains(a) {
		return errors.New("webhook: refused address")
	}
	return nil
}

// SafeClient is an internet subscriber's client: every dialled address passes Guard after
// resolution, so a name that rebinds to a private address is refused at connect; no redirect is
// followed, since the address it names was never declared.
func SafeClient(timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: timeout, Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		return Guard(ap.Addr())
	}}
	tr := &http.Transport{
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		Proxy:                 nil, // a proxy would dial on our behalf, past Guard
	}
	// Not httpx.New: its transport dials without Guard, and this dialer is the guard (L9). Lint
	// bans only http.DefaultClient and the http.Get family, so a client built here passes.
	return &http.Client{
		Timeout:       timeout,
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
