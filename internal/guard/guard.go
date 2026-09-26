// Package guard protects the server against browser-driven attacks that
// bypass network restrictions: DNS rebinding (Host check) and CSRF
// (custom header and Origin check on state-changing requests).
package guard

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CSRFHeader must be present (with value "1") on state-changing API requests.
const CSRFHeader = "X-Tether"

// Hosts is the set of host names accepted in the Host header.
type Hosts struct {
	names map[string]bool
}

// NewHosts builds a host allowlist. IP literals are always accepted.
func NewHosts(names ...string) *Hosts {
	h := &Hosts{names: map[string]bool{"localhost": true}}
	for _, n := range names {
		if n = normalize(n); n != "" {
			h.names[n] = true
		}
	}
	return h
}

// Names returns the configured names (for logging).
func (h *Hosts) Names() []string {
	out := make([]string, 0, len(h.names))
	for n := range h.names {
		out = append(out, n)
	}
	return out
}

// Allowed reports whether a Host header value (optionally with port) is acceptable.
func (h *Hosts) Allowed(hostport string) bool {
	host := hostport
	if hh, _, err := net.SplitHostPort(hostport); err == nil {
		host = hh
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	// DNSリバインディングは攻撃者が管理するドメイン名を必要とするため、IPリテラルは常に安全
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return h.names[normalize(host)]
}

func normalize(n string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(n)), ".")
}

// DetectNames returns this machine's host name and, when Tailscale is running,
// its MagicDNS name. Failures are ignored.
func DetectNames() []string {
	var names []string
	if hn, err := os.Hostname(); err == nil {
		names = append(names, hn)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "status", "--self", "--json").Output()
	if err != nil {
		return names
	}
	var st struct {
		Self struct {
			DNSName string
		}
	}
	if json.Unmarshal(out, &st) == nil && st.Self.DNSName != "" {
		names = append(names, st.Self.DNSName)
	}
	return names
}

// UnixForwardedHost restores the browser-facing host for requests that
// arrive on a Unix socket. tailscale serve rewrites Host to "localhost" when
// proxying to a Unix socket and passes the original in X-Forwarded-Host.
// Only the socket's owner and root can connect to it, so the header is trusted
// there; on TCP listeners it is ignored because any client could forge it.
// The restored host is still checked against the allowlist by Middleware.
func UnixForwardedHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fh := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0])
		if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && fh != "" {
			if _, isUnix := la.(*net.UnixAddr); isUnix {
				r = r.Clone(r.Context())
				r.Host = fh
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Middleware enforces the Host allowlist on every request and the CSRF rules
// on state-changing /api/ requests.
func Middleware(hosts *Hosts, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts.Allowed(r.Host) {
			log.Printf("[guard] rejected host %q from %s", r.Host, r.RemoteAddr)
			http.Error(w, "invalid host", http.StatusMisdirectedRequest)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && !safeMethod(r.Method) {
			// 独自ヘッダを付けたクロスオリジンのリクエストはCORSのプリフライトが必要になり、
			// tetherはCORSを許可しないので、他サイトのページからは送れない
			if r.Header.Get(CSRFHeader) != "1" {
				http.Error(w, "missing "+CSRFHeader+" header", http.StatusForbidden)
				return
			}
			if !SameOrigin(r) {
				http.Error(w, "cross-origin request", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// SameOrigin reports whether the request's Origin header, when present,
// matches its Host. Requests without Origin (non-browser clients) pass.
func SameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}
