package auth

import (
	"strings"
	"testing"

	"github.com/darshpatel1052/conduit/internal/config"
)

func newAuth(t *testing.T, keys []config.APIKey, enabled bool) *Authenticator {
	t.Helper()
	a, err := New(config.Auth{Enabled: enabled, Keys: keys}, config.RateLimit{
		DefaultRequestsPerMinute: 60,
		DefaultTokensPerMinute:   10000,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestGenerateKeyRoundTrip(t *testing.T) {
	plaintext, hash, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	if !strings.HasPrefix(plaintext, KeyPrefix) {
		t.Errorf("key %q lacks the %q prefix that lets secret scanners spot a leak", plaintext, KeyPrefix)
	}
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars of SHA-256", len(hash))
	}
	if HashKey(plaintext) != hash {
		t.Error("HashKey does not reproduce the hash returned by GenerateKey")
	}

	// Two calls must not collide.
	other, _, _ := GenerateKey()
	if other == plaintext {
		t.Error("GenerateKey returned the same key twice")
	}
}

func TestAuthenticateAcceptsValidKey(t *testing.T) {
	plaintext, hash, _ := GenerateKey()
	a := newAuth(t, []config.APIKey{{ID: "k1", Name: "test", Hash: hash}}, true)

	p, err := a.Authenticate("Bearer " + plaintext)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.ID != "k1" {
		t.Errorf("principal ID = %q, want k1", p.ID)
	}
	// Defaults should have been applied.
	if p.RequestsPerMinute != 60 || p.TokensPerMinute != 10000 {
		t.Errorf("limits not defaulted: rpm=%d tpm=%d", p.RequestsPerMinute, p.TokensPerMinute)
	}
}

func TestAuthenticateCarriesAdminCapability(t *testing.T) {
	plaintext, hash, _ := GenerateKey()
	a := newAuth(t, []config.APIKey{{ID: "ops", Hash: hash, Admin: true}}, true)

	p, err := a.Authenticate("Bearer " + plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Admin {
		t.Fatal("admin capability was lost while building the principal")
	}
}

func TestAuthenticateRejects(t *testing.T) {
	_, hash, _ := GenerateKey()
	a := newAuth(t, []config.APIKey{{ID: "k1", Hash: hash}}, true)

	tests := []struct{ name, header string }{
		{"empty", ""},
		{"wrong key", "Bearer ck_deadbeef"},
		{"scheme only", "Bearer "},
		{"garbage", "not-a-key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := a.Authenticate(tc.header); err == nil {
				t.Error("Authenticate accepted an invalid credential")
			}
		})
	}
}

// TestRejectionMessageIsUniform: distinguishing "no key" from "wrong key" tells
// an attacker probing for valid keys whether they are getting warmer.
func TestRejectionMessageIsUniform(t *testing.T) {
	_, hash, _ := GenerateKey()
	a := newAuth(t, []config.APIKey{{ID: "k1", Hash: hash}}, true)

	_, missing := a.Authenticate("")
	_, wrong := a.Authenticate("Bearer ck_wrong")

	if missing.Error() != wrong.Error() {
		t.Errorf("missing-key error %q differs from wrong-key error %q", missing, wrong)
	}
}

func TestDisabledKeyIsRejected(t *testing.T) {
	plaintext, hash, _ := GenerateKey()
	a := newAuth(t, []config.APIKey{{ID: "revoked", Hash: hash, Disabled: true}}, true)

	if _, err := a.Authenticate("Bearer " + plaintext); err == nil {
		t.Error("a disabled key was accepted; revocation does not work")
	}
}

func TestBareKeyWithoutSchemeIsAccepted(t *testing.T) {
	plaintext, hash, _ := GenerateKey()
	a := newAuth(t, []config.APIKey{{ID: "k1", Hash: hash}}, true)

	if _, err := a.Authenticate(plaintext); err != nil {
		t.Errorf("a bare key without the Bearer scheme was rejected: %v", err)
	}
}

func TestPerKeyLimitsOverrideDefaults(t *testing.T) {
	plaintext, hash, _ := GenerateKey()
	a := newAuth(t, []config.APIKey{{
		ID: "vip", Hash: hash, RequestsPerMinute: 5000, TokensPerMinute: 1000000,
	}}, true)

	p, err := a.Authenticate("Bearer " + plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if p.RequestsPerMinute != 5000 || p.TokensPerMinute != 1000000 {
		t.Errorf("per-key limits not applied: %+v", p)
	}
}

func TestCanUseModel(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		model   string
		want    bool
	}{
		{"no restriction", nil, "anything", true},
		{"explicit wildcard", []string{"*"}, "anything", true},
		{"exact match", []string{"gpt-4o"}, "gpt-4o", true},
		{"exact mismatch", []string{"gpt-4o"}, "claude-3", false},
		{"glob match", []string{"gpt-*"}, "gpt-4o-mini", true},
		{"glob mismatch", []string{"gpt-*"}, "claude-3", false},
		{"one of several", []string{"a", "b", "c"}, "b", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &Principal{Models: tc.allowed}
			if got := p.CanUseModel(tc.model); got != tc.want {
				t.Errorf("CanUseModel(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

func TestRateLimitKeySeparatesAnonymous(t *testing.T) {
	named := (&Principal{ID: "abc"}).RateLimitKey()
	anon := (&Principal{ID: "abc", Anonymous: true}).RateLimitKey()
	if named == anon {
		t.Error("an anonymous IP and a key with the same ID share a rate-limit bucket")
	}
}

func TestCustomHeaderAndScheme(t *testing.T) {
	plaintext, hash, _ := GenerateKey()
	a, err := New(config.Auth{
		Enabled: true, Header: "X-API-Key", Scheme: "",
		Keys: []config.APIKey{{ID: "k1", Hash: hash}},
	}, config.RateLimit{})
	if err != nil {
		t.Fatal(err)
	}

	if a.Header() != "X-API-Key" {
		t.Errorf("Header() = %q", a.Header())
	}
	if _, err := a.Authenticate(plaintext); err != nil {
		t.Errorf("raw key rejected with an empty scheme: %v", err)
	}
}

func TestConfigRejectsBadHash(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Enabled = true
	cfg.Auth.Keys = []config.APIKey{{ID: "k", Hash: "not-a-hash"}}

	if err := cfg.Validate(); err == nil {
		t.Error("Validate accepted a malformed key hash")
	}
}

func TestConfigRejectsAuthWithNoKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Enabled = true

	// Enabling auth with no keys would reject every request — almost certainly
	// a misconfiguration rather than an intent to serve nobody.
	if err := cfg.Validate(); err == nil {
		t.Error("Validate accepted auth.enabled with no keys")
	}
}
