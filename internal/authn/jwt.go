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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
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
	// ErrKeysStale is a token whose key was last confirmed by the issuer longer
	// ago than the stale cap: an outage long enough that a rotated-out key could
	// still be in our hands.
	ErrKeysStale = fmt.Errorf("%w: issuer keys past the stale cap", ErrUnauthenticated)
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
	// ClientID is where ZITADEL names the client in a JWT access token, which has no azp.
	ClientID string `json:"client_id,omitempty"`
	// ProjectRoles is the roles claim ZITADEL asserts natively: role name to granting orgs.
	ProjectRoles map[string]json.RawMessage `json:"urn:zitadel:iam:org:project:roles,omitempty"`
}

// GroupNames is the groups claim our ZITADEL action sets on ID tokens, else the
// project role names ZITADEL asserts natively, which is what a roomctl access
// token may carry instead (accessTokenRoleAssertion). Sorted, never nil.
func (c *Claims) GroupNames() []string {
	if len(c.Groups) > 0 {
		return c.Groups
	}
	out := slices.AppendSeq(make([]string, 0, len(c.ProjectRoles)), maps.Keys(c.ProjectRoles))
	slices.Sort(out)
	return out
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
	jwks   *jwksCache // nil for NewVerifierWithKeyfunc
}

// Option configures a Verifier.
type Option func(*options)

type options struct {
	client      *http.Client
	log         *slog.Logger
	now         func() time.Time
	ttl         time.Duration
	minInterval time.Duration
	maxStale    time.Duration
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

// WithMaxStale sets how long held keys keep verifying after the last
// successful fetch while the issuer is unreachable (default 24 h).
func WithMaxStale(d time.Duration) Option { return func(o *options) { o.maxStale = d } }

func newOptions(opts []Option) options {
	o := options{
		log: slog.New(slog.DiscardHandler), now: time.Now,
		ttl: time.Hour, minInterval: time.Minute, maxStale: 24 * time.Hour,
	}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// NewVerifier fetches the issuer's JWKS now, through the audited egress client,
// and refetches it on use once it is older than the cache TTL or when a token
// names an unknown kid, never more often than the minimum refresh interval.
// Held keys stop verifying once the last successful fetch is older than the
// stale cap. A JWKS that cannot be fetched or holds no usable key fails
// construction.
func NewVerifier(ctx context.Context, issuer, jwksURL string, opts ...Option) (*Verifier, error) {
	v, err := NewLazyVerifier(issuer, jwksURL, opts...)
	if err != nil {
		return nil, err
	}
	c := v.jwks
	c.sem <- struct{}{} // not shared yet: never blocks
	err = c.refreshHeld(ctx)
	<-c.sem
	if err != nil {
		return nil, fmt.Errorf("authn: initial JWKS fetch for %s: %w", issuer, err)
	}
	return v, nil
}

// NewLazyVerifier is NewVerifier without the fetch at construction: the first
// token or Refresh fetches the keys, and until then every token is refused. It
// suits an issuer the process must start without, such as the humans' IdP.
func NewLazyVerifier(issuer, jwksURL string, opts ...Option) (*Verifier, error) {
	if issuer == "" {
		return nil, errors.New("authn: an issuer is required")
	}
	o := newOptions(opts)
	c, err := newJWKSCache(jwksURL, o)
	if err != nil {
		return nil, err
	}
	return &Verifier{issuer: issuer, keys: c.lookup, now: o.now, jwks: c}, nil
}

// LastRefresh is when the issuer's keys were last fetched successfully; zero
// for a Verifier built on a key function. The metrics set gauges its age.
func (v *Verifier) LastRefresh() time.Time {
	if v.jwks == nil {
		return time.Time{}
	}
	return v.jwks.lastRefresh()
}

// Refresh fetches the issuer's JWKS now, unless a fetch started within the
// minimum refresh interval, which is no error (Ruling AQ: the broker calls it on
// a timer, so an idle broker's keys and LastRefresh stay fresh). It waits for a
// fetch already in flight. A failed fetch keeps the held keys, up to the stale
// cap, and leaves LastRefresh where it was. A Verifier on a key function has
// nothing to refresh.
func (v *Verifier) Refresh(ctx context.Context) error {
	if v.jwks == nil {
		return nil
	}
	return v.jwks.refresh(ctx)
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
	c, err := v.parse(ctx, raw, audience)
	if err != nil {
		return nil, err
	}
	// jwt matches when any aud element does. A token minted for several
	// audiences is refused: one presented here must be good for nothing else.
	if len(c.Audience) != 1 || c.Audience[0] != audience {
		return nil, fmt.Errorf("%w: want exactly one audience", ErrWrongAudience)
	}
	return c, nil
}

// parse checks raw's signature, algorithm, iss, exp, nbf and iat, and that one
// of its aud elements is audience. The callers add their own audience rules.
func (v *Verifier) parse(ctx context.Context, raw, audience string) (*Claims, error) {
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
	ClientID    string    // humans: the rooms client, the ID token's azp or roomctl's client_id (ruling P18)
	Expiry      time.Time // a connection lives min(exp, 1 h)
	AccessToken string    // humans only; forwarded to the factory (C4), never logged
}

// String is the principal's id, so %v and %+v never print AccessToken.
func (p Principal) String() string { return p.ID }

// GoString keeps AccessToken out of %#v.
func (p Principal) GoString() string {
	return fmt.Sprintf("authn.Principal{Kind:%q, ID:%q, RunID:%q}", p.Kind, p.ID, p.RunID)
}

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
