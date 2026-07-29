// Package oauth implements the provider OAuth flows — the tau port of Pi's
// packages/ai/src/auth/oauth. Request shapes (endpoints, client ids, scopes,
// parameter names and order) are ported verbatim: providers fingerprint them.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

// randomState returns a fresh CSRF state value: 16 random bytes as hex,
// matching Pi's createState.
//
// The state is what ties a redirect back to the login attempt that started it.
// A flow that reuses the verifier for both — as Anthropic's does — is fine
// because the verifier is already unguessable, but a separate value is clearer
// where the provider round-trips it visibly.
func randomState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
