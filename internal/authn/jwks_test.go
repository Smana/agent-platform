// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Smana/agent-platform/internal/httpx"
)

var b64 = base64.RawURLEncoding.EncodeToString

func rsaJWK(kid string, k *rsa.PublicKey) map[string]string {
	return map[string]string{"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
		"n": b64(k.N.Bytes()), "e": b64(big.NewInt(int64(k.E)).Bytes())}
}

func ecJWK(t *testing.T, kid, crv string, k *ecdsa.PublicKey) map[string]string {
	pt, err := k.Bytes() // 0x04 || X || Y
	if err != nil {
		t.Fatal(err)
	}
	n := (len(pt) - 1) / 2
	return map[string]string{"kty": "EC", "kid": kid, "crv": crv, "x": b64(pt[1 : 1+n]), "y": b64(pt[1+n:])}
}

// issuerServer serves a JWKS the test can swap, break and count.
type issuerServer struct {
	srv    *httptest.Server
	hits   atomic.Int32
	mu     sync.Mutex
	body   []byte
	status int
}

func newIssuerServer(t *testing.T, keys ...map[string]string) *issuerServer {
	s := &issuerServer{status: http.StatusOK}
	s.publish(t, keys...)
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.hits.Add(1)
		s.mu.Lock()
		defer s.mu.Unlock()
		w.WriteHeader(s.status)
		_, _ = w.Write(s.body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *issuerServer) publish(t *testing.T, keys ...map[string]string) {
	b, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		t.Fatal(err)
	}
	s.serve(http.StatusOK, b)
}

func (s *issuerServer) serve(status int, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.body = status, body
}

// client is the audited egress client, trusting only this server's CA.
func (s *issuerServer) client() *http.Client {
	roots := x509.NewCertPool()
	roots.AddCert(s.srv.Certificate())
	return httpx.New(5*time.Second, roots)
}

type keyring struct {
	rsa *rsa.PrivateKey
	ec  *ecdsa.PrivateKey
}

func newKeyring(t *testing.T) keyring {
	r, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return keyring{r, e}
}

func mint(t *testing.T, clock *fakeClock, m jwt.SigningMethod, key any, kid string) string {
	return sign(t, m, key, kid, jwt.RegisteredClaims{
		Issuer: issuer, Subject: runSub, Audience: jwt.ClaimStrings{AudienceRun},
		ExpiresAt: jwt.NewNumericDate(clock.now().Add(10 * time.Minute)),
	})
}

func newJWKSVerifier(t *testing.T, s *issuerServer, clock *fakeClock) *Verifier {
	v, err := NewVerifier(t.Context(), issuer, s.srv.URL, WithHTTPClient(s.client()), WithClock(clock.now),
		WithCacheTTL(time.Hour), WithMinRefreshInterval(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestJWKSVerifiesPublishedKeys(t *testing.T) {
	k := newKeyring(t)
	s := newIssuerServer(t, rsaJWK("r1", &k.rsa.PublicKey), ecJWK(t, "e1", "P-256", &k.ec.PublicKey))
	clock := newClock()
	v := newJWKSVerifier(t, s, clock)

	cases := map[string]struct {
		raw  string
		want error
	}{
		"RS256 on an RSA key":        {raw: mint(t, clock, jwt.SigningMethodRS256, k.rsa, "r1")},
		"ES256 on a P-256 key":       {raw: mint(t, clock, jwt.SigningMethodES256, k.ec, "e1")},
		"ES256 claiming the RSA kid": {raw: mint(t, clock, jwt.SigningMethodES256, k.ec, "r1"), want: ErrUnauthenticated},
		"RS256 claiming the EC kid":  {raw: mint(t, clock, jwt.SigningMethodRS256, k.rsa, "e1"), want: ErrUnauthenticated},
		"no kid":                     {raw: mint(t, clock, jwt.SigningMethodRS256, k.rsa, ""), want: ErrUnknownKey},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := v.Verify(t.Context(), c.raw, AudienceRun)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	// "no kid" is refused without asking the issuer.
	if got := s.hits.Load(); got != 1 {
		t.Fatalf("issuer fetched %d times, want 1 (the initial fetch)", got)
	}
}

// A flood of tokens with unknown kids costs the issuer one fetch per interval.
func TestAnUnknownKidRefreshesAtMostOncePerInterval(t *testing.T) {
	k, rotated := newKeyring(t), newKeyring(t)
	s := newIssuerServer(t, rsaJWK("r1", &k.rsa.PublicKey))
	clock := newClock()
	v := newJWKSVerifier(t, s, clock)
	clock.advance(time.Minute) // the initial fetch counts toward the interval

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			raw := mint(t, clock, jwt.SigningMethodRS256, rotated.rsa, "r2")
			if _, err := v.Verify(t.Context(), raw, AudienceRun); !errors.Is(err, ErrUnknownKey) {
				t.Errorf("err = %v, want ErrUnknownKey", err)
			}
		})
	}
	wg.Wait()
	if got := s.hits.Load(); got != 2 {
		t.Fatalf("issuer fetched %d times, want 2 (initial + one refresh)", got)
	}

	// The issuer rotates; within the interval the new key is still refused...
	s.publish(t, rsaJWK("r1", &k.rsa.PublicKey), rsaJWK("r2", &rotated.rsa.PublicKey))
	clock.advance(30 * time.Second)
	if _, err := v.Verify(t.Context(), mint(t, clock, jwt.SigningMethodRS256, rotated.rsa, "r2"), AudienceRun); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("within the interval: %v", err)
	}
	// ...and accepted once it has passed.
	clock.advance(31 * time.Second)
	if _, err := v.Verify(t.Context(), mint(t, clock, jwt.SigningMethodRS256, rotated.rsa, "r2"), AudienceRun); err != nil {
		t.Fatalf("after the interval: %v", err)
	}
	if got := s.hits.Load(); got != 3 {
		t.Fatalf("issuer fetched %d times, want 3", got)
	}
}

func TestTheCacheExpires(t *testing.T) {
	k := newKeyring(t)
	s := newIssuerServer(t, rsaJWK("r1", &k.rsa.PublicKey))
	clock := newClock()
	v := newJWKSVerifier(t, s, clock)

	clock.advance(59 * time.Minute)
	if _, err := v.Verify(t.Context(), mint(t, clock, jwt.SigningMethodRS256, k.rsa, "r1"), AudienceRun); err != nil {
		t.Fatal(err)
	}
	if got := s.hits.Load(); got != 1 {
		t.Fatalf("a fresh cache refetched: %d fetches", got)
	}

	t.Run("a key rotated out is refused after the TTL", func(t *testing.T) {
		other := newKeyring(t)
		s.publish(t, rsaJWK("r9", &other.rsa.PublicKey))
		clock.advance(2 * time.Minute)
		_, err := v.Verify(t.Context(), mint(t, clock, jwt.SigningMethodRS256, k.rsa, "r1"), AudienceRun)
		if !errors.Is(err, ErrUnknownKey) {
			t.Fatalf("err = %v, want ErrUnknownKey", err)
		}
		if got := s.hits.Load(); got != 2 {
			t.Fatalf("issuer fetched %d times, want 2", got)
		}
	})

	t.Run("a known key outlives an issuer outage", func(t *testing.T) {
		s.publish(t, rsaJWK("r1", &k.rsa.PublicKey))
		clock.advance(2 * time.Hour)
		if _, err := v.Verify(t.Context(), mint(t, clock, jwt.SigningMethodRS256, k.rsa, "r1"), AudienceRun); err != nil {
			t.Fatal(err)
		}
		s.serve(http.StatusServiceUnavailable, nil)
		clock.advance(2 * time.Hour)
		if _, err := v.Verify(t.Context(), mint(t, clock, jwt.SigningMethodRS256, k.rsa, "r1"), AudienceRun); err != nil {
			t.Fatalf("stale key refused during the outage: %v", err)
		}
	})
}

func TestUnusableKeysAreNeverLoaded(t *testing.T) {
	k := newKeyring(t)
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	enc := rsaJWK("enc", &k.rsa.PublicKey)
	enc["use"] = "enc"
	wrongAlg := rsaJWK("ps256", &k.rsa.PublicKey)
	wrongAlg["alg"] = "PS256"
	s := newIssuerServer(t, rsaJWK("r1", &k.rsa.PublicKey), rsaJWK("small", &small.PublicKey),
		ecJWK(t, "p384", "P-384", &p384.PublicKey), enc, wrongAlg,
		map[string]string{"kty": "oct", "kid": "hmac", "k": b64([]byte("shared-secret"))})
	clock := newClock()
	v := newJWKSVerifier(t, s, clock)

	cases := map[string]string{
		"RSA under 2048 bits":  mint(t, clock, jwt.SigningMethodRS256, small, "small"),
		"P-384":                mint(t, clock, jwt.SigningMethodES384, p384, "p384"),
		"use enc":              mint(t, clock, jwt.SigningMethodRS256, k.rsa, "enc"),
		"alg other than RS256": mint(t, clock, jwt.SigningMethodRS256, k.rsa, "ps256"),
		"symmetric key":        mint(t, clock, jwt.SigningMethodHS256, []byte("shared-secret"), "hmac"),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Verify(t.Context(), raw, AudienceRun); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
	if _, err := v.Verify(t.Context(), mint(t, clock, jwt.SigningMethodRS256, k.rsa, "r1"), AudienceRun); err != nil {
		t.Fatalf("the usable key beside them: %v", err)
	}
}

func TestNewVerifierRefuses(t *testing.T) {
	k := newKeyring(t)
	good := rsaJWK("r1", &k.rsa.PublicKey)
	tooMany := make([]map[string]string, maxJWKSKeys+1)
	for i := range tooMany {
		tooMany[i] = rsaJWK("k"+strings.Repeat("x", i), &k.rsa.PublicKey)
	}
	huge, err := json.Marshal(map[string]any{"keys": []any{good}, "pad": strings.Repeat("x", maxJWKSBytes)})
	if err != nil {
		t.Fatal(err)
	}
	dup, err := json.Marshal(map[string]any{"keys": []any{good, good}})
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		status  int
		body    []byte
		keys    []map[string]string
		wantErr error
	}{
		"a non-200 answer":       {status: http.StatusNotFound, body: []byte(`{}`)},
		"a body over the cap":    {status: http.StatusOK, body: huge, wantErr: httpx.ErrBodyTooLarge},
		"more keys than the cap": {keys: tooMany},
		"a duplicate kid":        {status: http.StatusOK, body: dup},
		"no usable key":          {keys: []map[string]string{{"kty": "oct", "kid": "h", "k": "c2VjcmV0"}}},
		"not JSON":               {status: http.StatusOK, body: []byte("<html>")},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := newIssuerServer(t, c.keys...)
			if c.body != nil {
				s.serve(c.status, c.body)
			}
			_, err := NewVerifier(t.Context(), issuer, s.srv.URL, WithHTTPClient(s.client()))
			if err == nil {
				t.Fatal("accepted")
			}
			if c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
		})
	}

	t.Run("a plain-http JWKS URL", func(t *testing.T) {
		if _, err := NewVerifier(t.Context(), issuer, "http://issuer.example/keys"); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("no issuer", func(t *testing.T) {
		if _, err := NewVerifier(t.Context(), "", "https://issuer.example/keys"); err == nil {
			t.Fatal("accepted")
		}
	})
}
