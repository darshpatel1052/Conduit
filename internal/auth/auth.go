// Package auth authenticates callers by API key.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"github.com/darshpatel1052/conduit/internal/config"
)

// KeyPrefix marks Conduit-issued keys. A recognisable prefix lets secret
// scanners (GitHub's, and most commercial ones) detect a leaked key in a commit
// and revoke it before it is abused.
const KeyPrefix = "ck_"

// Principal is an authenticated caller.
type Principal struct {
	ID    string
	Name  string
	Admin bool

	// Project-scoped fields are populated by database-backed application keys.
	// Keeping the static fields above allows old development configurations to
	// remain readable while project-managed keys become the default path.
	KeyID       string
	ProjectID   string
	Environment string

	RequestsPerMinute        int
	TokensPerMinute          int
	ProjectRequestsPerMinute int
	ProjectTokensPerMinute   int
	EndUserRequestsPerMinute *int
	RequireEndUser           bool
	CacheMode                string
	PolicyVersion            int64
	EndUserPseudonym         string
	MonthlyBudgetUSD         *float64
	BudgetWarningPercent     int
	CacheSemanticThreshold   float64
	CacheMaxTemperature      float64
	CacheTTLSeconds          int

	// Models optionally restricts callable routes; empty means unrestricted.
	Models []string

	// Anonymous is true when auth is disabled and the caller was identified by
	// IP rather than by credential.
	Anonymous bool
}

// CanUseModel reports whether this principal may call the given model.
func (p *Principal) CanUseModel(model string) bool {
	if len(p.Models) == 0 {
		return true
	}
	for _, allowed := range p.Models {
		if allowed == "*" || allowed == model {
			return true
		}
		if ok, err := path.Match(allowed, model); err == nil && ok {
			return true
		}
	}
	return false
}

// RateLimitKey identifies this principal in the limiter's keyspace.
func (p *Principal) RateLimitKey() string {
	if p.Anonymous {
		return "anon:" + p.ID
	}
	return "key:" + p.ID
}

// Authenticator validates credentials against the configured key set.
type Authenticator struct {
	enabled bool
	header  string
	scheme  string

	// byHash indexes keys by their SHA-256. Lookup is by hash, never by
	// plaintext comparison against a list, so the number of configured keys
	// does not affect how long a rejection takes.
	byHash map[string]*Principal

	defaultRPM int
	defaultTPM int
	resolver   Resolver
}

type Resolver interface {
	ResolveAPIKey(ctx context.Context, token, sourceIP string) (*Principal, error)
}

// New builds an authenticator from config.
func New(cfg config.Auth, rl config.RateLimit) (*Authenticator, error) {
	a := &Authenticator{
		enabled:    cfg.Enabled,
		header:     cfg.Header,
		scheme:     cfg.Scheme,
		byHash:     make(map[string]*Principal, len(cfg.Keys)),
		defaultRPM: rl.DefaultRequestsPerMinute,
		defaultTPM: rl.DefaultTokensPerMinute,
	}
	if a.header == "" {
		a.header = "Authorization"
	}
	if a.scheme == "" {
		a.scheme = "Bearer"
	}

	for _, k := range cfg.Keys {
		if k.Disabled {
			continue
		}
		hash := strings.ToLower(strings.TrimSpace(k.Hash))
		if _, err := hex.DecodeString(hash); err != nil {
			return nil, fmt.Errorf("auth key %q: hash is not valid hex: %w", k.ID, err)
		}

		p := &Principal{
			ID:                k.ID,
			Name:              k.Name,
			Admin:             k.Admin,
			Models:            k.Models,
			RequestsPerMinute: k.RequestsPerMinute,
			TokensPerMinute:   k.TokensPerMinute,
		}
		if p.RequestsPerMinute == 0 {
			p.RequestsPerMinute = a.defaultRPM
		}
		if p.TokensPerMinute == 0 {
			p.TokensPerMinute = a.defaultTPM
		}
		a.byHash[hash] = p
	}
	return a, nil
}

// Enabled reports whether credentials are required.
func (a *Authenticator) Enabled() bool { return a.enabled || a.resolver != nil }

func (a *Authenticator) SetResolver(resolver Resolver) { a.resolver = resolver }

// Header returns the header credentials are read from.
func (a *Authenticator) Header() string { return a.header }

// ErrUnauthorized is returned for a missing or invalid credential. The message
// is deliberately identical in both cases: distinguishing "no key" from "wrong
// key" tells an attacker probing for valid keys whether they are close.
var ErrUnauthorized = fmt.Errorf("invalid or missing API key")

// Authenticate resolves a raw header value to a principal.
func (a *Authenticator) Authenticate(headerValue string) (*Principal, error) {
	token := a.extractToken(headerValue)
	if token == "" {
		return nil, ErrUnauthorized
	}

	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])

	principal, ok := a.byHash[hash]
	if !ok {
		// Burn a comparison even on a miss so that a lookup failure and a
		// lookup success take indistinguishable time.
		var dummy [32]byte
		subtle.ConstantTimeCompare(sum[:], dummy[:])
		return nil, ErrUnauthorized
	}
	return principal, nil
}

func (a *Authenticator) AuthenticateRequest(ctx context.Context, headerValue, sourceIP string) (*Principal, error) {
	token := a.extractToken(headerValue)
	if token == "" {
		return nil, ErrUnauthorized
	}
	if a.resolver != nil && strings.HasPrefix(token, "ck_") && strings.Count(token, "_") == 2 {
		return a.resolver.ResolveAPIKey(ctx, token, sourceIP)
	}
	return a.Authenticate(headerValue)
}

func (a *Authenticator) extractToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if a.scheme == "" {
		return value
	}
	prefix := a.scheme + " "
	if len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
		return strings.TrimSpace(value[len(prefix):])
	}
	// Tolerate a bare key without the scheme; some clients send it that way.
	return value
}

// AnonymousPrincipal builds a principal for unauthenticated traffic, keyed by
// client IP so rate limits still apply when auth is off.
func (a *Authenticator) AnonymousPrincipal(ip string, rpm int) *Principal {
	return &Principal{
		ID:                ip,
		Name:              "anonymous",
		Anonymous:         true,
		RequestsPerMinute: rpm,
	}
}

// GenerateKey mints a new API key, returning the plaintext and its hash.
//
// The plaintext is returned exactly once, at creation. Nothing stores it — the
// config keeps only the hash — so a leaked config file cannot be turned into
// working credentials.
func GenerateKey() (plaintext, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate key: %w", err)
	}
	plaintext = KeyPrefix + hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plaintext))
	return plaintext, hex.EncodeToString(sum[:]), nil
}

// HashKey returns the stored form of a plaintext key.
func HashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// HashKeyBytes is the binary representation used by PostgreSQL-backed keys.
func HashKeyBytes(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// CompareKeyHash performs the final secret comparison after indexed lookup by
// the key's public identifier.
func CompareKeyHash(plaintext string, stored []byte) bool {
	sum := sha256.Sum256([]byte(plaintext))
	return len(stored) == len(sum) && subtle.ConstantTimeCompare(sum[:], stored) == 1
}

type ctxKey struct{}

// WithPrincipal attaches the caller to the request context.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom retrieves the authenticated caller, if any.
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(*Principal)
	return p, ok && p != nil
}
