// SPDX-License-Identifier: Apache-2.0

// Package authn authenticates every caller offline (programme C2 r5): an
// audience-bound JWT, checked against an allowlisted issuer's JWKS. No
// TokenReview: liveness comes from the AgentRun itself (runwatch).
//
// Every refusal wraps ErrUnauthenticated; the finer sentinels (ErrTokenExpired,
// ErrWrongAudience, ...) say why. Neither a token nor its claims ever reach a log
// line or an error message.
package authn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Smana/agent-platform/internal/envelope"
)

// The audiences this broker accepts.
const (
	AudienceRun    = "room-broker"  // fixed by SP1 (C2); Kyverno reserves it to namespace agents
	AudienceSystem = "rooms-system" // ruling P3
)

var (
	// ErrUnauthenticated is every refusal of a credential: missing, malformed or
	// invalid. APIs map it to 401.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrForbidden is a valid credential for a principal outside the allowlist. APIs map it to 403.
	ErrForbidden = errors.New("forbidden")

	// ErrTokenExpired is a token past exp plus the allowed skew.
	ErrTokenExpired = fmt.Errorf("%w: token expired", ErrUnauthenticated)
	// ErrTokenNotYetValid is a token before nbf or iat, minus the allowed skew.
	ErrTokenNotYetValid = fmt.Errorf("%w: token not yet valid", ErrUnauthenticated)
	// ErrWrongIssuer is a token from an issuer this Verifier does not trust.
	ErrWrongIssuer = fmt.Errorf("%w: wrong issuer", ErrUnauthenticated)
	// ErrWrongAudience is a token minted for another audience.
	ErrWrongAudience = fmt.Errorf("%w: wrong audience", ErrUnauthenticated)
	// ErrUnknownKey is a token whose kid the issuer's JWKS does not publish.
	ErrUnknownKey = fmt.Errorf("%w: unknown signing key", ErrUnauthenticated)
)

// validMethods pins the algorithms: the asymmetric ones Kubernetes and ZITADEL
// sign with. Never none, never HS*: an HS256 token keyed with a public key
// anyone can read would otherwise verify.
var validMethods = []string{jwt.SigningMethodRS256.Alg(), jwt.SigningMethodES256.Alg()}

// clockSkew bounds the tolerance on exp, nbf and iat.
const clockSkew = 30 * time.Second

// Claims is what a verified token asserts.
type Claims struct {
	jwt.RegisteredClaims
	Groups          []string `json:"groups,omitempty"`
	AuthorizedParty string   `json:"azp,omitempty"`
}

// keyLookup returns the key a token claims to be signed with. It may fetch, so
// it takes the request's context.
type keyLookup func(ctx context.Context, t *jwt.Token) (any, error)

// Verifier checks tokens from one issuer: signature, algorithm, iss, aud, exp,
// nbf and iat, with a bounded skew. It is safe for concurrent use.
type Verifier struct {
	issuer string
	keys   keyLookup
	now    func() time.Time
}

// Option configures a Verifier.
type Option func(*options)

type options struct {
	client      *http.Client
	log         *slog.Logger
	now         func() time.Time
	ttl         time.Duration
	minInterval time.Duration
}

// WithClock replaces time.Now for token expiry and the JWKS cache.
func WithClock(now func() time.Time) Option { return func(o *options) { o.now = now } }

// WithLogger sets where JWKS refreshes are reported. The default discards.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.log = l } }

// WithHTTPClient replaces the JWKS client; pass an httpx client, for example one
// trusting a private CA. The default is httpx.New with the system pool.
func WithHTTPClient(c *http.Client) Option { return func(o *options) { o.client = c } }

// WithCacheTTL sets how long a fetched JWKS is trusted before a refetch (default 1 h).
func WithCacheTTL(d time.Duration) Option { return func(o *options) { o.ttl = d } }

// WithMinRefreshInterval sets the least time between two JWKS fetches (default
// 1 min), so a flood of unknown kids cannot hammer the issuer.
func WithMinRefreshInterval(d time.Duration) Option {
	return func(o *options) { o.minInterval = d }
}

func newOptions(opts []Option) options {
	o := options{
		log: slog.New(slog.DiscardHandler), now: time.Now,
		ttl: time.Hour, minInterval: time.Minute,
	}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// NewVerifier fetches the issuer's JWKS now, through the audited egress client,
// and refetches it on use once it is older than the cache TTL or when a token
// names an unknown kid, never more often than the minimum refresh interval. A
// JWKS that cannot be fetched or holds no usable key fails construction.
func NewVerifier(ctx context.Context, issuer, jwksURL string, opts ...Option) (*Verifier, error) {
	if issuer == "" {
		return nil, errors.New("authn: an issuer is required")
	}
	o := newOptions(opts)
	c, err := newJWKSCache(jwksURL, o)
	if err != nil {
		return nil, err
	}
	if err := c.refresh(ctx, o.now()); err != nil {
		return nil, fmt.Errorf("authn: initial JWKS fetch for %s: %w", issuer, err)
	}
	return &Verifier{issuer: issuer, keys: c.lookup, now: o.now}, nil
}

// NewVerifierWithKeyfunc builds a Verifier on a caller's key function, for tests
// and for issuers whose keys are not a JWKS. Only WithClock applies.
func NewVerifierWithKeyfunc(issuer string, kf jwt.Keyfunc, opts ...Option) *Verifier {
	o := newOptions(opts)
	return &Verifier{issuer: issuer, now: o.now,
		keys: func(_ context.Context, t *jwt.Token) (any, error) { return kf(t) }}
}

// Verify checks raw for audience and returns its claims. A refusal wraps
// ErrUnauthenticated and, where one applies, a finer sentinel.
func (v *Verifier) Verify(ctx context.Context, raw, audience string) (*Claims, error) {
	if audience == "" {
		return nil, fmt.Errorf("%w: no audience to check", ErrUnauthenticated)
	}
	c := &Claims{}
	_, err := jwt.ParseWithClaims(raw, c, func(t *jwt.Token) (any, error) {
		// Before any key lookup, so another issuer's kid never refreshes this JWKS.
		if iss, err := t.Claims.GetIssuer(); err != nil || iss != v.issuer {
			return nil, ErrWrongIssuer
		}
		return v.keys(ctx, t)
	},
		jwt.WithValidMethods(validMethods), jwt.WithIssuer(v.issuer), jwt.WithAudience(audience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(clockSkew), jwt.WithTimeFunc(v.now))
	if err != nil {
		return nil, classify(err)
	}
	return c, nil
}

// classify maps the jwt library's errors onto this package's sentinels, keeping
// the original in the chain.
func classify(err error) error {
	var sentinel error
	switch {
	case errors.Is(err, ErrUnauthenticated): // from our key lookup, already classified
		return err
	case errors.Is(err, jwt.ErrTokenExpired):
		sentinel = ErrTokenExpired
	case errors.Is(err, jwt.ErrTokenNotValidYet), errors.Is(err, jwt.ErrTokenUsedBeforeIssued):
		sentinel = ErrTokenNotYetValid
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		sentinel = ErrWrongIssuer
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		sentinel = ErrWrongAudience
	default:
		sentinel = ErrUnauthenticated
	}
	return fmt.Errorf("%w: %w", sentinel, err)
}

// Principal is who is calling, in the C2 canonical form.
type Principal struct {
	Kind        envelope.ActorKind
	ID          string // agent:<runId> | human:<sub> | system:<component>
	RunID       string
	Sub         string
	Groups      []string
	ClientID    string    // azp, for humans (ruling P18)
	Expiry      time.Time // a connection lives min(exp, 1 h)
	AccessToken string    // humans only; forwarded to the factory (C4), never logged
}

// String is the principal's id, so %v and %+v never print AccessToken.
func (p Principal) String() string { return p.ID }

// LogValue keeps AccessToken out of every log line that carries a Principal.
func (p Principal) LogValue() slog.Value {
	return slog.GroupValue(slog.String("kind", string(p.Kind)), slog.String("id", p.ID),
		slog.String("runId", p.RunID), slog.Time("expiry", p.Expiry))
}

// Bearer returns the request's bearer token. The scheme is case-insensitive (RFC 9110).
func Bearer(r *http.Request) (string, error) {
	scheme, raw, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || raw == "" {
		return "", fmt.Errorf("%w: no bearer token", ErrUnauthenticated)
	}
	return raw, nil
}
