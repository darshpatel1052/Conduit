package control

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

type Secrets struct {
	key []byte
}

func ParseMasterKey(encoded string) (*Secrets, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("CONDUIT_MASTER_KEY must be base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("CONDUIT_MASTER_KEY must decode to exactly 32 bytes, got %d", len(key))
	}
	return &Secrets{key: key}, nil
}

func (s *Secrets) Encrypt(projectID, providerID, plaintext string) ([]byte, error) {
	if plaintext == "" {
		return nil, nil
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate credential nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, []byte(plaintext), associatedData(projectID, providerID)), nil
}

func (s *Secrets) Decrypt(projectID, providerID string, ciphertext []byte) (string, error) {
	if len(ciphertext) == 0 {
		return "", nil
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return "", errors.New("encrypted credential is truncated")
	}
	nonce, body := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, body, associatedData(projectID, providerID))
	if err != nil {
		return "", errors.New("encrypted credential failed authentication")
	}
	return string(plain), nil
}

func (s *Secrets) EndUserPseudonym(projectID, raw string) string {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("conduit:end-user:" + projectID + "\x00" + raw))
	return hex.EncodeToString(mac.Sum(nil))
}

func associatedData(projectID, providerID string) []byte {
	return []byte("conduit:provider:" + projectID + ":" + providerID)
}

func CredentialFingerprint(secret string) string {
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:6])
}
