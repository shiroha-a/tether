package peer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestClientTokenAndPolicy(t *testing.T) {
	s, dir := newTestStore(t)
	c, token, err := s.AddClient("  laptop  ", Policy{Status: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "laptop" || len(token) != 64 || c.TokenHash == token || c.TokenHash == "" {
		t.Fatalf("client = %+v token=%q", c, token)
	}
	// 許可モードは既定でdefault
	if c.Policy.DelegateMode != "default" {
		t.Fatalf("delegate mode = %q", c.Policy.DelegateMode)
	}
	// トークンはファイルに平文で残らない
	b, _ := os.ReadFile(filepath.Join(dir, "peers.json"))
	if strings.Contains(string(b), token) {
		t.Fatal("token stored in plain text")
	}
	info, _ := os.Stat(filepath.Join(dir, "peers.json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v", info.Mode().Perm())
	}

	got, ok := s.Authenticate(token)
	if !ok || got.ID != c.ID {
		t.Fatalf("authenticate: %v %+v", ok, got)
	}
	for _, bad := range []string{"", "x", token[:63], token + "0", c.TokenHash} {
		if _, ok := s.Authenticate(bad); ok {
			t.Errorf("token %q accepted", bad)
		}
	}

	// 名前の重複・空・長すぎ・制御文字
	for _, name := range []string{"laptop", " ", strings.Repeat("a", 41), "a\nb"} {
		if _, _, err := s.AddClient(name, Policy{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q: %v", name, err)
		}
	}
	// bypassPermissionsは依頼の許可モードに使えない
	if _, _, err := s.AddClient("x", Policy{Delegate: true, DelegateMode: "bypassPermissions"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bypass accepted: %v", err)
	}

	up, err := s.SetPolicy(c.ID, Policy{Exec: true, DelegateMode: "acceptEdits"})
	if err != nil || !up.Policy.Exec || up.Policy.Status || up.Policy.DelegateMode != "acceptEdits" {
		t.Fatalf("set policy: %+v %v", up, err)
	}
	if _, err := s.SetPolicy(c.ID, Policy{DelegateMode: "bypassPermissions"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("set bypass: %v", err)
	}
	if _, err := s.SetPolicy("missing", Policy{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set missing: %v", err)
	}

	// 読み直しても同じ。削除したらトークンは使えない
	re, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := re.Authenticate(token); !ok || !got.Policy.Exec {
		t.Fatalf("reloaded: %v %+v", ok, got)
	}
	if err := re.DeleteClient(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := re.Authenticate(token); ok {
		t.Fatal("deleted client still authenticates")
	}
	if err := re.DeleteClient(c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestLastSeenIsSavedAtMostEveryMinute(t *testing.T) {
	s, dir := newTestStore(t)
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	c, token, _ := s.AddClient("a", Policy{})
	saved := func() time.Time {
		re, _ := NewStore(dir)
		return re.Clients()[0].LastSeenAt
	}
	s.Authenticate(token)
	if !saved().Equal(now) {
		t.Fatalf("first seen not saved: %v", saved())
	}
	first := now
	now = now.Add(30 * time.Second)
	got, _ := s.Authenticate(token)
	// メモリ上は更新するが、ファイルへの保存は1分たつまでしない
	if !got.LastSeenAt.Equal(now) || !saved().Equal(first) {
		t.Fatalf("memory %v, file %v", got.LastSeenAt, saved())
	}
	now = now.Add(31 * time.Second)
	s.Authenticate(token)
	if !saved().Equal(now) {
		t.Fatalf("not saved after a minute: %v", saved())
	}
	_ = c
}

func TestRemotes(t *testing.T) {
	s, dir := newTestStore(t)
	r, err := s.AddRemote("vps", "https://vps.example.ts.net:3100/", "tok", Policy{Status: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.URL != "https://vps.example.ts.net:3100" || r.Token != "tok" {
		t.Fatalf("remote = %+v", r)
	}
	if _, err := s.AddRemote("local", "http://localhost:3198", "tok", Policy{}); err != nil {
		t.Fatalf("http localhost: %v", err)
	}
	for _, u := range []string{
		"http://vps.example:3100",         // 平文のhttpはlocalhost以外で使わない
		"ftp://vps.example",               // スキーム
		"https://",                        // ホストなし
		"https://u:p@vps.example",         // 認証情報つき
		"https://vps.example/tether",      // パスつき
		"https://vps.example/?x=1",        // クエリつき
		"vps.example:3100",                // スキームなし
		"http://127.0.0.2:3100",           // loopbackでもlocalhost以外の表記は受け付けない
		"http://localhost.evil.example:1", // 名前の前方一致
	} {
		if _, err := s.AddRemote("x", u, "tok", Policy{}); !errors.Is(err, ErrInvalid) {
			t.Errorf("url %q: %v", u, err)
		}
	}
	if _, err := s.AddRemote("vps", "https://other.example", "tok", Policy{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := s.AddRemote("y", "https://other.example", "", Policy{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty token: %v", err)
	}
	if got, ok := s.Remote("vps"); !ok || got.ID != r.ID {
		t.Fatalf("by name: %v %+v", ok, got)
	}
	if got, ok := s.RemoteByID(r.ID); !ok || got.Name != "vps" {
		t.Fatalf("by id: %v %+v", ok, got)
	}
	up, err := s.SetRemotePolicy(r.ID, Policy{Exec: true})
	if err != nil || !up.Policy.Exec {
		t.Fatalf("set remote policy: %+v %v", up, err)
	}
	re, _ := NewStore(dir)
	if got, _ := re.Remote("vps"); !got.Policy.Exec || got.Token != "tok" {
		t.Fatalf("reloaded remote = %+v", got)
	}
	if err := re.DeleteRemote(r.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := re.Remote("vps"); ok {
		t.Fatal("deleted remote still found")
	}
	if err := re.DeleteRemote(r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if _, err := re.SetRemotePolicy("missing", Policy{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set missing: %v", err)
	}
}

func TestDelegatedIsCapped(t *testing.T) {
	s, _ := newTestStore(t)
	c, _, _ := s.AddClient("a", Policy{})
	for i := range maxDelegated + 5 {
		if err := s.AddDelegated(c.ID, strings.Repeat("x", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	d := s.Clients()[0].Delegated
	if len(d) != maxDelegated || d[0] != strings.Repeat("x", 6) || d[len(d)-1] != strings.Repeat("x", maxDelegated+5) {
		t.Fatalf("delegated len=%d first=%d last=%d", len(d), len(d[0]), len(d[len(d)-1]))
	}
	if err := s.AddDelegated("missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing client: %v", err)
	}
}

func TestPairingCode(t *testing.T) {
	code := PairingCode("https://b.example:3100", "abc")
	if !strings.HasPrefix(code, "tether-pair:") {
		t.Fatalf("code = %q", code)
	}
	u, tok, err := ParsePairingCode("  " + code + "\n")
	if err != nil || u != "https://b.example:3100" || tok != "abc" {
		t.Fatalf("parse: %q %q %v", u, tok, err)
	}
	// 中身が正しくても、接頭辞のないものは受け付けない
	if _, _, err := ParsePairingCode(strings.TrimPrefix(code, "tether-pair:")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("code without prefix: %v", err)
	}
	for _, bad := range []string{"", "abc", "tether-pair:!!!", "tether-pair:" + "e30", PairingCode("", "abc"), PairingCode("https://b", "")} {
		if _, _, err := ParsePairingCode(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("code %q: %v", bad, err)
		}
	}
}

func TestLoadBrokenFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "peers.json"), []byte("{"), 0o600)
	if _, err := NewStore(dir); err == nil {
		t.Fatal("broken file accepted")
	}
}
