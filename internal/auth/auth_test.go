package auth

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	secret := []byte("test-secret")
	tok := Mint(secret, "alice", time.Minute)

	sub, err := Verify(secret, tok)
	if err != nil || sub != "alice" {
		t.Fatalf("round trip: sub=%q err=%v", sub, err)
	}

	if _, err := Verify([]byte("wrong-secret"), tok); err == nil {
		t.Error("wrong secret accepted")
	}
	if _, err := Verify(secret, tok[:len(tok)-2]); err == nil {
		t.Error("truncated signature accepted")
	}
	if _, err := Verify(secret, Mint(secret, "alice", -time.Minute)); err == nil {
		t.Error("expired token accepted")
	}
	if _, err := Verify(secret, "not.a.jwt"); err == nil {
		t.Error("garbage accepted")
	}

	// alg:none with a valid-looking body must be rejected.
	b64 := base64.RawURLEncoding
	parts := strings.Split(tok, ".")
	forged := b64.EncodeToString([]byte(`{"alg":"none"}`)) + "." + parts[1] + "."
	if _, err := Verify(secret, forged); err == nil {
		t.Error("alg:none accepted")
	}
}
