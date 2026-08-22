package control

import (
	"encoding/base64"
	"strings"
	"testing"
)

func testSecrets(t *testing.T) *Secrets {
	t.Helper()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	secrets, err := ParseMasterKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return secrets
}

func TestCredentialEncryptionBindsProjectAndProvider(t *testing.T) {
	s := testSecrets(t)
	ciphertext, err := s.Encrypt("project-a", "provider-a", "secret-value")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := s.Decrypt("project-a", "provider-a", ciphertext)
	if err != nil || plain != "secret-value" {
		t.Fatalf("round trip: plain=%q err=%v", plain, err)
	}
	if _, err := s.Decrypt("project-b", "provider-a", ciphertext); err == nil {
		t.Fatal("ciphertext moved to another project should fail authentication")
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := s.Decrypt("project-a", "provider-a", ciphertext); err == nil {
		t.Fatal("tampered ciphertext should fail authentication")
	}
}

func TestEndUserPseudonymsAreStableAndProjectScoped(t *testing.T) {
	s := testSecrets(t)
	first := s.EndUserPseudonym("project-a", "user-42")
	if first != s.EndUserPseudonym("project-a", "user-42") {
		t.Fatal("same project and identifier must be stable")
	}
	if first == s.EndUserPseudonym("project-b", "user-42") {
		t.Fatal("same identifier must not correlate across projects")
	}
	if strings.Contains(first, "user-42") {
		t.Fatal("pseudonym contains raw identifier")
	}
}

func TestMasterKeyLengthIsRequired(t *testing.T) {
	_, err := ParseMasterKey(base64.StdEncoding.EncodeToString([]byte("too-short")))
	if err == nil {
		t.Fatal("short key accepted")
	}
}

func TestPasswordHashAndVerification(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hash, "correct horse") {
		t.Fatal("hash contains password")
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Fatal("wrong password accepted")
	}
}

func TestProviderURLPolicy(t *testing.T) {
	for _, raw := range []string{"https://api.openai.com", "http://localhost:11434", "http://mock-provider:19100", "http://10.0.0.4:8080"} {
		if err := ValidateProviderURL("openai", raw); err != nil {
			t.Errorf("%s rejected: %v", raw, err)
		}
	}
	for _, raw := range []string{"file:///etc/passwd", "http://example.com", "https://user:pass@example.com"} {
		if err := ValidateProviderURL("openai", raw); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
}
