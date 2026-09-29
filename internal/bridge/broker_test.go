// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// GP-18: the bridge trusts exactly the broker's CA, over https only, and
// refuses to start otherwise.
func TestNewBrokerRefusesAnInsecureOrMissingTrustRoot(t *testing.T) {
	dir := t.TempDir()
	tok := writeToken(t, dir, "v1")
	_, ca := (&fakeBroker{}).start(t)
	notPEM := filepath.Join(dir, "not-a-ca")
	if err := os.WriteFile(notPEM, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, base, ca string
	}{
		{"plain http", "http://room-broker:8443", ca},
		{"no scheme", "room-broker:8443", ca},
		{"no host", "https://", ca},
		{"a CA file that is absent", "https://room-broker:8443", filepath.Join(dir, "absent.crt")},
		{"a CA file with no certificate", "https://room-broker:8443", notPEM},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if b, err := NewBroker(c.base, tok, c.ca); err == nil {
				t.Fatalf("NewBroker accepted it: %+v", b)
			}
		})
	}
}

// otherCA writes a self-signed CA that signed nothing the tests serve.
func otherCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "another CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "other-ca.crt")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBrokerTrustsOnlyItsCA(t *testing.T) {
	tok := writeToken(t, t.TempDir(), "v1")
	srv, ourCA := (&fakeBroker{}).start(t)
	cases := []struct {
		name    string
		ca      string
		trusted bool
	}{
		{"the server's CA is in the file", ourCA, true},
		{"the server's CA is not in the file", otherCA(t), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := NewBroker(srv.URL, tok, c.ca)
			if err != nil {
				t.Fatal(err)
			}
			_, rep, err := b.Hello(t.Context())
			var unknown x509.UnknownAuthorityError
			switch {
			case c.trusted && (err != nil || rep.Code != http.StatusOK):
				t.Fatalf("hello = %d, %v", rep.Code, err)
			case !c.trusted && !errors.As(err, &unknown):
				t.Fatalf("hello to an untrusted server: %d, %v", rep.Code, err)
			}
		})
	}
}

func TestTheTokenIsReReadBeforeEveryRequest(t *testing.T) {
	fb := &fakeBroker{}
	srv, ca := fb.start(t)
	dir := t.TempDir()
	b, err := NewBroker(srv.URL, writeToken(t, dir, "v1"), ca)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"v1", "v2", "v3"} {
		writeToken(t, dir, v)
		if _, _, err := b.Hello(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if got := strings.Join(fb.tokens, ","); got != "Bearer v1,Bearer v2,Bearer v3" {
		t.Fatalf("tokens sent: %s", got)
	}
}

func TestARefusalCarriesItsReasonAndRetryAfter(t *testing.T) {
	fb := &fakeBroker{events: []reply{{code: http.StatusTooManyRequests, reason: "rate_limited", retryAfter: "3"}}}
	srv, ca := fb.start(t)
	b, err := NewBroker(srv.URL, writeToken(t, t.TempDir(), "v1"), ca)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := b.Send(t.Context(), nil)
	if err != nil || rep.Code != http.StatusTooManyRequests || rep.Reason != "rate_limited" || rep.RetryAfter != "3" {
		t.Fatalf("%+v %v", rep, err)
	}
}
