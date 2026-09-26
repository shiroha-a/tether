// Package listen opens the server listener (TCP or a private Unix socket) and
// builds the matching client for hook callbacks.
package listen

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const unixPrefix = "unix:"

// Listen opens addr. "unix:<path>" creates a Unix socket readable and writable
// only by the current user; anything else is a TCP address.
func Listen(addr string) (net.Listener, error) {
	path, ok := strings.CutPrefix(addr, unixPrefix)
	if !ok {
		return net.Listen("tcp", addr)
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("unix socket path must be absolute: %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := removeStale(path); err != nil {
		return nil, err
	}
	// 作成直後にchmodすると、その一瞬だけ他ユーザーから接続できる隙間ができるので、作成時点で0600にする
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	return ln, nil
}

// removeStale deletes a socket file left by a previous run. It refuses to
// touch a socket that is still served or a path that is not a socket.
func removeStale(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	if c, err := net.DialTimeout("unix", path, 500*time.Millisecond); err == nil {
		c.Close()
		return fmt.Errorf("%s is in use (is tether already running?)", path)
	}
	return os.Remove(path)
}

// HookTarget returns the value for `tether hook --url` that reaches ln.
func HookTarget(ln net.Listener) string {
	switch a := ln.Addr().(type) {
	case *net.UnixAddr:
		return unixPrefix + a.Name
	case *net.TCPAddr:
		return fmt.Sprintf("http://127.0.0.1:%d/internal/hook", a.Port)
	default:
		return ""
	}
}

// HookClient returns an HTTP client and the request URL for a hook target
// produced by HookTarget.
func HookClient(target string) (*http.Client, string) {
	path, ok := strings.CutPrefix(target, unixPrefix)
	if !ok {
		return http.DefaultClient, target
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
	// ホスト名はUnixソケットでは使われないが、Hostヘッダ検証を通るようlocalhostにする
	return &http.Client{Transport: tr}, "http://localhost/internal/hook"
}

// IsUnix reports whether the connection was accepted on a Unix socket.
func IsUnix(local net.Addr) bool {
	_, ok := local.(*net.UnixAddr)
	return ok
}
