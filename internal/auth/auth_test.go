package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddleware(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := Middleware("secret", ok)

	cases := []struct {
		name   string
		url    string
		header string
		want   int
	}{
		{"no token", "/api/x", "", http.StatusUnauthorized},
		{"wrong query token", "/api/x?token=nope", "", http.StatusUnauthorized},
		{"empty query token", "/api/x?token=", "", http.StatusUnauthorized},
		{"wrong bearer", "/api/x", "Bearer nope", http.StatusUnauthorized},
		{"prefix of token", "/api/x?token=secre", "", http.StatusUnauthorized},
		{"query token", "/api/x?token=secret", "", http.StatusNoContent},
		{"bearer token", "/api/x", "Bearer secret", http.StatusNoContent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, c.url, nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
		})
	}
}

func TestMiddlewareDisabledWhenTokenEmpty(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	rec := httptest.NewRecorder()
	Middleware("", ok).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}

func TestCheckRejectsEmptyToken(t *testing.T) {
	// Checkは公開関数なので、設定トークンが空でも空の入力を一致とみなさないこと
	if Check("", httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Fatal("empty token matched empty configuration")
	}
	if Check("", httptest.NewRequest(http.MethodGet, "/?token=", nil)) {
		t.Fatal("empty query token matched empty configuration")
	}
}
