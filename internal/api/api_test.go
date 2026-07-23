package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Validation rejections happen before any AWS call, so a zero-value Server is
// enough to exercise the trust boundary.
func TestCreateJobValidation(t *testing.T) {
	h := NewServer(nil, nil, nil, nil, Config{RatePerMinute: 10}).Handler()
	cases := []struct {
		name, user, body string
		want             int
	}{
		{"missing user", "", `{"prompt":"x"}`, 400},
		{"bad json", "u1", `{`, 400},
		{"empty prompt", "u1", `{"prompt":"  "}`, 400},
		{"oversize prompt", "u1", `{"prompt":"` + strings.Repeat("a", 2000) + `"}`, 400},
		{"bad priority", "u1", `{"prompt":"x","priority":"urgent"}`, 400},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/jobs", strings.NewReader(c.body))
		if c.user != "" {
			req.Header.Set("X-User-Id", c.user)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != c.want {
			t.Errorf("%s: got %d, want %d (body: %s)", c.name, w.Code, c.want, w.Body)
		}
	}
}
