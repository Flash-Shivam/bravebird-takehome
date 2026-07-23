package api

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shivamjadhav/bravebird-takehome/internal/auth"
)

// Validation and auth rejections happen before any AWS call, so a zero-value
// Server is enough to exercise the trust boundary.
func TestCreateJobValidation(t *testing.T) {
	secret := []byte("test-secret")
	h := NewServer(nil, nil, nil, nil, Config{RatePerMinute: 10, JWTSecret: secret}).Handler()
	good := "Bearer " + auth.Mint(secret, "u1", time.Minute)
	cases := []struct {
		name, authz, body string
		want              int
	}{
		{"missing token", "", `{"prompt":"x"}`, 401},
		{"garbage token", "Bearer not.a.jwt", `{"prompt":"x"}`, 401},
		{"expired token", "Bearer " + auth.Mint(secret, "u1", -time.Minute), `{"prompt":"x"}`, 401},
		{"wrong secret", "Bearer " + auth.Mint([]byte("other"), "u1", time.Minute), `{"prompt":"x"}`, 401},
		{"bad json", good, `{`, 400},
		{"empty prompt", good, `{"prompt":"  "}`, 400},
		{"oversize prompt", good, `{"prompt":"` + strings.Repeat("a", 2000) + `"}`, 400},
		{"bad priority", good, `{"prompt":"x","priority":"urgent"}`, 400},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/jobs", strings.NewReader(c.body))
		if c.authz != "" {
			req.Header.Set("Authorization", c.authz)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != c.want {
			t.Errorf("%s: got %d, want %d (body: %s)", c.name, w.Code, c.want, w.Body)
		}
	}
}

func TestHealthzNoAuth(t *testing.T) {
	h := NewServer(nil, nil, nil, nil, Config{JWTSecret: []byte("s")}).Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 {
		t.Errorf("healthz should not require auth: got %d", w.Code)
	}
}
