// Package oauth implements the provider OAuth flows — the tau port of Pi's
// packages/ai/src/auth/oauth. Request shapes (endpoints, client ids, scopes,
// parameter names and order) are ported verbatim: providers fingerprint them.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// PKCE is a code verifier and its S256 challenge.
type PKCE struct {
	Verifier  string
	Challenge string
}

// GeneratePKCE creates a 32-byte random verifier (base64url, unpadded) and its
// SHA-256 challenge, matching Pi's pkce.ts.
func GeneratePKCE() (PKCE, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return PKCE{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	return PKCE{Verifier: verifier, Challenge: Challenge(verifier)}, nil
}

// Challenge returns the S256 challenge for a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
