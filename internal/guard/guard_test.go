package guard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostsAllowed(t *testing.T) {
	h := NewHosts("devbox", "devbox.example-tailnet.ts.net.", " Proxy.Example.com ")
	cases := []struct {
		host string
		want bool
	}{
		{"localhost:3100", true},
		{"LOCALHOST", true},
		{"127.0.0.1:3100", true},
		{"[::1]:3100", true},
		{"100.64.1.2:3100", true},
		{"[fd7a:115c:a1e0::2]:3100", true},
		{"devbox:3100", true},
		{"devbox.example-tailnet.ts.net:3100", true},
		{"devbox.example-tailnet.ts.net.:3100", true},
		{"proxy.example.com", true},
		{"evil.example.com:3100", false},
		{"devbox.evil.example", false},
		{"localhost.evil.example", false},
		{"127.0.0.1.nip.io:3100", false},
		{"", false},
	}
	for _, c := range cases {
		if got := h.Allowed(c.host); got != c.want {
			t.Errorf("Allowed(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestMiddleware(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := Middleware(NewHosts("devbox"), ok)

	type req struct {
		name, method, path, host, origin string
		csrf                             bool
		want                             int
	}
	cases := []req{
		{"rebinding host GET", "GET", "/api/sessions", "evil.example:3100", "", false, http.StatusMisdirectedRequest},
		{"rebinding host static", "GET", "/", "evil.example:3100", "", false, http.StatusMisdirectedRequest},
		{"rebinding host with header", "POST", "/api/sessions", "evil.example:3100", "http://evil.example:3100", true, http.StatusMisdirectedRequest},
		{"GET needs no header", "GET", "/api/sessions", "devbox:3100", "", false, http.StatusNoContent},
		{"POST without header", "POST", "/api/sessions", "devbox:3100", "", false, http.StatusForbidden},
		{"upload without header", "POST", "/api/fs/upload", "127.0.0.1:3100", "http://evil.example", false, http.StatusForbidden},
		{"DELETE without header", "DELETE", "/api/sessions/x", "devbox:3100", "", false, http.StatusForbidden},
		{"PATCH without header", "PATCH", "/api/sessions/x", "devbox:3100", "", false, http.StatusForbidden},
		{"POST with header, no origin", "POST", "/api/sessions", "devbox:3100", "", true, http.StatusNoContent},
		{"POST with header, same origin", "POST", "/api/sessions", "devbox:3100", "http://devbox:3100", true, http.StatusNoContent},
		{"POST with header, cross origin", "POST", "/api/sessions", "devbox:3100", "http://evil.example", true, http.StatusForbidden},
		{"POST with header, other port", "POST", "/api/sessions", "devbox:3100", "http://devbox:9999", true, http.StatusForbidden},
		{"POST with header, null origin", "POST", "/api/sessions", "devbox:3100", "null", true, http.StatusForbidden},
		{"hook is exempt", "POST", "/internal/hook", "127.0.0.1:3100", "", false, http.StatusNoContent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, c.path, nil)
			r.Host = c.host
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if c.csrf {
				r.Header.Set(CSRFHeader, "1")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
		})
	}
}

func TestCSRFHeaderValueMustBeOne(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r := httptest.NewRequest("POST", "/api/sessions", nil)
	r.Host = "localhost:3100"
	r.Header.Set(CSRFHeader, "0")
	rec := httptest.NewRecorder()
	Middleware(NewHosts(), ok).ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestUnixForwardedHost(t *testing.T) {
	var seen string
	h := UnixForwardedHost(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.Host }))
	do := func(local net.Addr, xfh string) string {
		r := httptest.NewRequest("POST", "/api/sessions", nil)
		r.Host = "localhost"
		if xfh != "" {
			r.Header.Set("X-Forwarded-Host", xfh)
		}
		if local != nil {
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
		}
		seen = ""
		h.ServeHTTP(httptest.NewRecorder(), r)
		return seen
	}
	unix := &net.UnixAddr{Name: "/run/t.sock", Net: "unix"}
	tcp := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3100}
	// tailscale serveがUnixソケットへ転送するときは、元のホスト名がX-Forwarded-Hostに入る
	if got := do(unix, "devbox.example.ts.net:3100"); got != "devbox.example.ts.net:3100" {
		t.Fatalf("unix: Host = %q", got)
	}
	if got := do(unix, "a.example:3100, b.example"); got != "a.example:3100" {
		t.Fatalf("unix with list: Host = %q", got)
	}
	// TCPでは誰でもヘッダを付けられるので使わない
	if got := do(tcp, "evil.example"); got != "localhost" {
		t.Fatalf("tcp: Host = %q, header must be ignored", got)
	}
	if got := do(unix, ""); got != "localhost" {
		t.Fatalf("unix without header: Host = %q", got)
	}
	if got := do(nil, "evil.example"); got != "localhost" {
		t.Fatalf("no local addr: Host = %q", got)
	}
}

func TestUnixForwardedHostIsStillValidated(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := UnixForwardedHost(Middleware(NewHosts("devbox"), ok))
	unix := &net.UnixAddr{Name: "/run/t.sock", Net: "unix"}
	post := func(xfh, origin string) int {
		r := httptest.NewRequest("POST", "/api/sessions", nil)
		r.Host = "localhost"
		r.Header.Set("X-Forwarded-Host", xfh)
		r.Header.Set("Origin", origin)
		r.Header.Set(CSRFHeader, "1")
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, unix))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	// 元のホスト名とOriginが一致すれば通る（修正前はHostがlocalhostのままでCSRF検証に失敗していた）
	if c := post("devbox:3100", "https://devbox:3100"); c != http.StatusNoContent {
		t.Fatalf("same origin via unix socket: %d", c)
	}
	// 復元したホスト名も許可リストで検証される
	if c := post("evil.example:3100", "https://evil.example:3100"); c != http.StatusMisdirectedRequest {
		t.Fatalf("unlisted forwarded host: %d", c)
	}
}
