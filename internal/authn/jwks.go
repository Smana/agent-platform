// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Smana/agent-platform/internal/httpx"
)

// Bounds on what an issuer may make us hold. Real issuers publish a handful of
// keys in a few KiB.
const (
	maxJWKSBytes = 256 << 10
	maxJWKSKeys  = 64
	minRSABits   = 2048
	jwksTimeout  = 10 * time.Second
)

// publicKey is a usable JWK: the key and the one algorithm it verifies.
type publicKey struct {
	key any
	alg string
}

// jwksCache holds one issuer's keys. The mutex is held across a fetch on
// purpose: concurrent misses wait for that one fetch instead of each starting
// their own. The fetch is bounded by the client's timeout.
type jwksCache struct {
	url         string
	client      *http.Client
	log         *slog.Logger
	now         func() time.Time
	ttl         time.Duration
	minInterval time.Duration

	mu        sync.Mutex
	keys      map[string]publicKey
	fetched   time.Time // last successful fetch
	attempted time.Time // last fetch, successful or not
}

func newJWKSCache(jwksURL string, o options) (*jwksCache, error) {
	u, err := url.Parse(jwksURL)
	if err != nil {
		return nil, fmt.Errorf("authn: JWKS URL: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("authn: the JWKS URL must be an absolute https:// URL")
	}
	if o.client == nil {
		o.client = httpx.New(jwksTimeout, nil)
	}
	return &jwksCache{url: jwksURL, client: o.client, log: o.log, now: o.now, ttl: o.ttl, minInterval: o.minInterval}, nil
}

// lookup is the Verifier's key function: the key a token's kid names, if it
// verifies the token's algorithm.
func (c *jwksCache) lookup(ctx context.Context, t *jwt.Token) (any, error) {
	kid, _ := t.Header["kid"].(string)
	if kid == "" {
		return nil, fmt.Errorf("%w: no kid", ErrUnknownKey)
	}
	k, err := c.get(ctx, kid)
	if err != nil {
		return nil, err
	}
	// The method is already one of validMethods, so naming it leaks nothing.
	if alg := t.Method.Alg(); alg != k.alg {
		return nil, fmt.Errorf("%w: the key verifies %s, not %s", ErrUnauthenticated, k.alg, alg)
	}
	return k.key, nil
}

func (c *jwksCache) get(ctx context.Context, kid string) (publicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	k, known := c.keys[kid]
	if known && now.Sub(c.fetched) < c.ttl {
		return k, nil
	}
	if now.Sub(c.attempted) < c.minInterval {
		if known {
			return k, nil
		}
		c.log.DebugContext(ctx, "jwks refresh rate-limited", "url", c.url)
		return publicKey{}, fmt.Errorf("%w: refresh rate-limited", ErrUnknownKey)
	}
	if err := c.refresh(ctx, now); err != nil {
		// A key we already hold outlives an issuer outage; an unknown one cannot.
		c.log.WarnContext(ctx, "jwks refresh failed", "url", c.url, "stale", known, "error", err)
		if known {
			return k, nil
		}
		return publicKey{}, fmt.Errorf("%w: %w", ErrUnknownKey, err)
	}
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	return publicKey{}, ErrUnknownKey
}

// refresh fetches and replaces the key set. Callers hold mu, except
// NewVerifier, which runs it before the cache is shared.
func (c *jwksCache) refresh(ctx context.Context, now time.Time) error {
	c.attempted = now
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("jwks request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("jwks fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks fetch: status %d", resp.StatusCode)
	}
	body, err := httpx.ReadBody(resp.Body, maxJWKSBytes)
	if err != nil {
		return fmt.Errorf("jwks fetch: %w", err)
	}
	keys, err := parseJWKS(body)
	if err != nil {
		return err
	}
	c.keys, c.fetched = keys, now
	c.log.DebugContext(ctx, "jwks refreshed", "url", c.url, "keys", len(keys))
	return nil
}

type rawJWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// parseJWKS keeps the keys it can use and skips the rest (another algorithm, an
// encryption key); a set with no usable key, too many keys or a repeated kid is
// refused whole.
func parseJWKS(body []byte) (map[string]publicKey, error) {
	var set struct {
		Keys []rawJWK `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("jwks decode: %w", err)
	}
	if len(set.Keys) > maxJWKSKeys {
		return nil, fmt.Errorf("jwks: %d keys, over the cap of %d", len(set.Keys), maxJWKSKeys)
	}
	keys := make(map[string]publicKey, len(set.Keys))
	seen := make(map[string]bool, len(set.Keys))
	for _, r := range set.Keys {
		if seen[r.Kid] {
			return nil, errors.New("jwks: a kid appears twice")
		}
		seen[r.Kid] = true
		if k, err := r.parse(); err == nil {
			keys[r.Kid] = k
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("jwks: no usable signing key")
	}
	return keys, nil
}

func (r rawJWK) parse() (publicKey, error) {
	if r.Kid == "" {
		return publicKey{}, errors.New("no kid")
	}
	if r.Use != "" && r.Use != "sig" {
		return publicKey{}, errors.New("not a signing key")
	}
	switch r.Kty {
	case "RSA":
		return r.parseRSA()
	case "EC":
		return r.parseEC()
	default:
		return publicKey{}, errors.New("unsupported key type")
	}
}

func (r rawJWK) parseRSA() (publicKey, error) {
	if r.Alg != "" && r.Alg != jwt.SigningMethodRS256.Alg() {
		return publicKey{}, errors.New("unsupported algorithm")
	}
	n, err := base64.RawURLEncoding.DecodeString(r.N)
	if err != nil {
		return publicKey{}, fmt.Errorf("n: %w", err)
	}
	e, err := base64.RawURLEncoding.DecodeString(r.E)
	if err != nil {
		return publicKey{}, fmt.Errorf("e: %w", err)
	}
	mod := new(big.Int).SetBytes(n)
	exp := new(big.Int).SetBytes(e)
	if mod.BitLen() < minRSABits {
		return publicKey{}, errors.New("RSA key too short")
	}
	if !exp.IsInt64() || exp.Int64() < 3 || exp.Int64() > math.MaxInt32 || exp.Bit(0) == 0 {
		return publicKey{}, errors.New("bad RSA exponent")
	}
	return publicKey{key: &rsa.PublicKey{N: mod, E: int(exp.Int64())}, alg: jwt.SigningMethodRS256.Alg()}, nil
}

func (r rawJWK) parseEC() (publicKey, error) {
	if r.Crv != "P-256" || (r.Alg != "" && r.Alg != jwt.SigningMethodES256.Alg()) {
		return publicKey{}, errors.New("unsupported curve or algorithm")
	}
	x, err := base64.RawURLEncoding.DecodeString(r.X)
	if err != nil {
		return publicKey{}, fmt.Errorf("x: %w", err)
	}
	y, err := base64.RawURLEncoding.DecodeString(r.Y)
	if err != nil {
		return publicKey{}, fmt.Errorf("y: %w", err)
	}
	if len(x) != 32 || len(y) != 32 {
		return publicKey{}, errors.New("bad P-256 coordinates")
	}
	// Rejects a point off the curve.
	k, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
	if err != nil {
		return publicKey{}, fmt.Errorf("P-256 point: %w", err)
	}
	return publicKey{key: k, alg: jwt.SigningMethodES256.Alg()}, nil
}
