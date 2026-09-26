// Package auth provides bearer-token authentication middleware.
package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Middleware rejects requests that do not carry the expected token.
// An empty token disables authentication.
func Middleware(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Check(token, r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Check reports whether the request carries the token in the Authorization
// header or the "token" query parameter.
func Check(token string, r *http.Request) bool {
	got := r.URL.Query().Get("token")
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		got = strings.TrimPrefix(h, "Bearer ")
	}
	// 空文字同士の一致で素通りしないよう、長さ0は常に拒否する
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}
