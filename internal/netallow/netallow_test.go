package netallow

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func mustParse(t *testing.T, spec string) *Policy {
	t.Helper()
	p, err := Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultPolicy(t *testing.T) {
	p := mustParse(t, "loopback,tailscale")
	a := netip.MustParseAddr
	cases := []struct {
		name          string
		remote, local string
		want          bool
	}{
		{"ipv4 loopback", "127.0.0.1", "127.0.0.1", true},
		{"ipv6 loopback", "::1", "::1", true},
		{"v4-mapped loopback", "::ffff:127.0.0.1", "::ffff:127.0.0.1", true},
		{"tailscale peer", "100.101.102.103", "100.64.1.2", true},
		{"tailscale ipv6 peer", "fd7a:115c:a1e0::1", "fd7a:115c:a1e0::2", true},
		{"v4-mapped tailscale", "::ffff:100.101.102.103", "::ffff:100.64.1.2", true},
		{"cgnat on LAN interface", "100.101.102.103", "192.168.1.10", false},
		{"LAN peer", "192.168.1.20", "192.168.1.10", false},
		{"LAN peer to tailscale ip", "192.168.1.20", "100.64.1.2", false},
		{"internet peer", "203.0.113.5", "192.168.1.10", false},
		{"just outside 100.64/10", "100.128.0.1", "100.64.1.2", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := p.Allowed(a(c.remote), a(c.local)); got != c.want {
				t.Fatalf("Allowed(%s, %s) = %v, want %v", c.remote, c.local, got, c.want)
			}
		})
	}
	if p.Allowed(netip.Addr{}, a("127.0.0.1")) {
		t.Fatal("invalid remote allowed")
	}
}

func TestCIDRAndAll(t *testing.T) {
	p := mustParse(t, "192.168.1.0/24, 10.0.0.5")
	a := netip.MustParseAddr
	if !p.Allowed(a("192.168.1.99"), a("192.168.1.10")) || !p.Allowed(a("10.0.0.5"), a("10.0.0.1")) {
		t.Fatal("configured CIDR/IP not allowed")
	}
	if p.Allowed(a("10.0.0.6"), a("10.0.0.1")) || p.Allowed(a("127.0.0.1"), a("127.0.0.1")) {
		t.Fatal("address outside the list allowed")
	}
	if !mustParse(t, "all").Allowed(a("203.0.113.5"), a("192.168.1.10")) {
		t.Fatal("all did not allow")
	}
}

func TestParseErrors(t *testing.T) {
	// 正しい項目にタイプミスが混ざっても黙って無視せず、起動時にエラーにする
	for _, spec := range []string{"", " , ", "lan", "300.1.1.1/8", "loopback,lan", "tailscale,10.0.0.0/33"} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) accepted", spec)
		}
	}
}

func TestMiddlewareUsesConnectionAddressesNotHeaders(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := mustParse(t, "loopback,tailscale").Middleware(ok)
	do := func(remote, local string, xff string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remote
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		ctx := context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP(local), Port: 3100})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req.WithContext(ctx))
		return rec.Code
	}
	if c := do("127.0.0.1:5000", "127.0.0.1", ""); c != http.StatusNoContent {
		t.Fatalf("loopback: %d", c)
	}
	if c := do("100.101.102.103:5000", "100.64.1.2", ""); c != http.StatusNoContent {
		t.Fatalf("tailscale: %d", c)
	}
	if c := do("192.168.1.20:5000", "192.168.1.10", "127.0.0.1"); c != http.StatusForbidden {
		t.Fatalf("LAN with spoofed X-Forwarded-For: %d", c)
	}
}

func TestMiddlewareAllowsUnixSocket(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := mustParse(t, "loopback,tailscale").Middleware(ok)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "@"
	ctx := context.WithValue(req.Context(), http.LocalAddrContextKey, &net.UnixAddr{Name: "/run/t.sock", Net: "unix"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unix socket request: %d", rec.Code)
	}
}
