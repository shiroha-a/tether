// Package netallow restricts which peers may connect, by source address.
package netallow

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

var tailscaleRanges = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// Policy decides whether a connection is allowed.
type Policy struct {
	all       bool
	loopback  bool
	tailscale bool
	prefixes  []netip.Prefix
}

// Parse builds a policy from a comma-separated list of "loopback",
// "tailscale", "all" and CIDRs (a bare IP is treated as a single-host CIDR).
func Parse(spec string) (*Policy, error) {
	p := &Policy{}
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		switch item {
		case "":
			continue
		case "all":
			p.all = true
		case "loopback", "localhost":
			p.loopback = true
		case "tailscale":
			p.tailscale = true
		default:
			pfx, err := netip.ParsePrefix(item)
			if err != nil {
				addr, aerr := netip.ParseAddr(item)
				if aerr != nil {
					return nil, fmt.Errorf("invalid allow entry %q", item)
				}
				pfx = netip.PrefixFrom(addr, addr.BitLen())
			}
			p.prefixes = append(p.prefixes, pfx.Masked())
		}
	}
	if !p.all && !p.loopback && !p.tailscale && len(p.prefixes) == 0 {
		return nil, fmt.Errorf("allow list is empty")
	}
	return p, nil
}

// Allowed reports whether a peer at remote, connected to our local address, may proceed.
func (p *Policy) Allowed(remote, local netip.Addr) bool {
	if p.all {
		return true
	}
	remote, local = remote.Unmap(), local.Unmap()
	if !remote.IsValid() {
		return false
	}
	if p.loopback && remote.IsLoopback() {
		return true
	}
	// 100.64.0.0/10はISPのCGNATでも使われるため、tailscaleインターフェース経由（ローカル側もtailscaleの帯）に限る
	if p.tailscale && inTailscale(remote) && inTailscale(local) {
		return true
	}
	for _, pfx := range p.prefixes {
		if pfx.Contains(remote) {
			return true
		}
	}
	return false
}

func inTailscale(a netip.Addr) bool {
	for _, r := range tailscaleRanges {
		if r.Contains(a) {
			return true
		}
	}
	return false
}

// Middleware rejects requests from disallowed peers with 403.
// X-Forwarded-For is deliberately ignored: it is client-controlled.
func (p *Policy) Middleware(next http.Handler) http.Handler {
	if p.all {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote := addrOf(r.RemoteAddr)
		var local netip.Addr
		if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			// Unixソケットはファイルの権限（所有者のみ）で接続できる相手が絞られているので許可する
			if _, isUnix := la.(*net.UnixAddr); isUnix {
				next.ServeHTTP(w, r)
				return
			}
			local = addrOf(la.String())
		}
		if !p.Allowed(remote, local) {
			log.Printf("[netallow] rejected %s -> %s %s", r.RemoteAddr, local, r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func addrOf(hostport string) netip.Addr {
	ap, err := netip.ParseAddrPort(hostport)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr()
}
