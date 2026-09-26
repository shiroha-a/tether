package listen

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sockPath(t *testing.T) string {
	t.Helper()
	// Unixソケットのパスは長さ制限（約108バイト）があるので、短い一時ディレクトリを使う
	dir, err := os.MkdirTemp("/tmp", "tl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "sub", "t.sock")
}

func TestUnixSocketIsPrivate(t *testing.T) {
	p := sockPath(t)
	ln, err := Listen("unix:" + p)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want socket 0600", info.Mode())
	}
	dir, _ := os.Stat(filepath.Dir(p))
	if dir.Mode().Perm() != 0o700 {
		t.Fatalf("created parent dir mode = %v, want 0700", dir.Mode().Perm())
	}
	// umaskは元に戻っていること（後から作るファイルに影響させない）
	f := filepath.Join(filepath.Dir(p), "after")
	os.WriteFile(f, nil, 0o644)
	if fi, _ := os.Stat(f); fi.Mode().Perm() == 0o600 {
		t.Fatal("umask was not restored")
	}
}

func TestStaleSocketIsReplacedButLiveOneIsNot(t *testing.T) {
	p := sockPath(t)
	ln, err := Listen("unix:" + p)
	if err != nil {
		t.Fatal(err)
	}
	// 使用中のソケットは奪わない
	if _, err := Listen("unix:" + p); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("second listen on live socket: %v", err)
	}
	// 異常終了で残ったソケット（誰も待ち受けていない）は置き換える
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	ln2, err := Listen("unix:" + p)
	if err != nil {
		t.Fatalf("listen over stale socket: %v", err)
	}
	ln2.Close()
}

func TestRefusesToDeleteNonSocket(t *testing.T) {
	p := sockPath(t)
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte("important"), 0o644)
	if _, err := Listen("unix:" + p); err == nil {
		t.Fatal("listened over a regular file")
	}
	if b, _ := os.ReadFile(p); string(b) != "important" {
		t.Fatal("regular file was modified")
	}
}

func TestRelativeUnixPathRejected(t *testing.T) {
	if _, err := Listen("unix:rel.sock"); err == nil {
		t.Fatal("relative socket path accepted")
	}
}

func TestHookClientReachesUnixSocket(t *testing.T) {
	p := sockPath(t)
	ln, err := Listen("unix:" + p)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		unix := IsUnix(r.Context().Value(http.LocalAddrContextKey).(net.Addr))
		got <- r.Host + " " + r.URL.Path + "?" + r.URL.RawQuery + " " + string(b) + " unix=" + map[bool]string{true: "yes", false: "no"}[unix]
	})}
	go srv.Serve(ln)
	defer srv.Close()

	target := HookTarget(ln)
	if target != "unix:"+p {
		t.Fatalf("HookTarget = %q", target)
	}
	client, u := HookClient(target)
	res, err := client.Post(u+"?sid=s1", "application/json", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if g := <-got; g != `localhost /internal/hook?sid=s1 {"a":1} unix=yes` {
		t.Fatalf("server saw %q", g)
	}
}

func TestTCPFallback(t *testing.T) {
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	target := HookTarget(ln)
	if !strings.HasPrefix(target, "http://127.0.0.1:") || !strings.HasSuffix(target, "/internal/hook") {
		t.Fatalf("HookTarget = %q", target)
	}
	if c, u := HookClient(target); c != http.DefaultClient || u != target {
		t.Fatalf("HookClient(tcp) = %v %q", c, u)
	}
	if IsUnix(ln.Addr()) {
		t.Fatal("tcp addr reported as unix")
	}
}

func TestEndpoint(t *testing.T) {
	c, u := Endpoint("http://127.0.0.1:3100/internal/hook", "/internal/mcp")
	if u != "http://127.0.0.1:3100/internal/mcp" || c != http.DefaultClient {
		t.Fatalf("tcp endpoint = %q", u)
	}
	c, u = Endpoint("unix:/run/t.sock", "/internal/mcp")
	if u != "http://localhost/internal/mcp" || c == http.DefaultClient {
		t.Fatalf("unix endpoint = %q", u)
	}
	if _, u := HookClient("unix:/run/t.sock"); u != "http://localhost/internal/hook" {
		t.Fatalf("hook url = %q", u)
	}
}
