// Package auth issues, resolves, and revokes virtual keys: the credentials
// clients present in the Authorization header, which the gateway resolves
// to an org/team/key identity.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

const keyPrefixLabel = "sk-vk-"

// generateKey returns a new plaintext virtual key. It is never stored —
// only its sha256 hash and an 8-char display prefix are.
func generateKey() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: generate key: %w", err)
	}
	return keyPrefixLabel + base64.RawURLEncoding.EncodeToString(raw), nil
}

func hashKey(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

func displayPrefix(plaintext string) string {
	if len(plaintext) <= 8 {
		return plaintext
	}
	return plaintext[:8]
}

func cacheKey(hash []byte) string {
	return "vk:" + hex.EncodeToString(hash)
}
