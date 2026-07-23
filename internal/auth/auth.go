// Package auth mints and verifies HS256 JWTs with stdlib only. The algorithm
// is pinned server-side, so alg-confusion attacks ("none", RS256-with-HMAC)
// are rejected by construction.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var b64 = base64.RawURLEncoding

type claims struct {
	Sub string `json:"sub"`
	Exp int64  `json:"exp"`
}

// Mint returns a signed token for sub, valid for ttl.
func Mint(secret []byte, sub string, ttl time.Duration) string {
	header := b64.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, _ := json.Marshal(claims{Sub: sub, Exp: time.Now().Add(ttl).Unix()})
	signing := header + "." + b64.EncodeToString(payload)
	return signing + "." + sign(secret, signing)
}

// Verify checks the signature and expiry and returns the subject.
func Verify(secret []byte, token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed token")
	}
	var hdr struct {
		Alg string `json:"alg"`
	}
	hdrBytes, err := b64.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hdrBytes, &hdr) != nil || hdr.Alg != "HS256" {
		return "", errors.New("bad header")
	}
	if !hmac.Equal([]byte(sign(secret, parts[0]+"."+parts[1])), []byte(parts[2])) {
		return "", errors.New("bad signature")
	}
	payload, err := b64.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("bad payload")
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return "", fmt.Errorf("bad claims: %w", err)
	}
	if c.Sub == "" {
		return "", errors.New("missing sub")
	}
	if c.Exp == 0 || time.Now().Unix() >= c.Exp {
		return "", errors.New("token expired")
	}
	return c.Sub, nil
}

func sign(secret []byte, msg string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	return b64.EncodeToString(mac.Sum(nil))
}
