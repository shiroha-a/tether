package peer

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeDelegator struct {
	calls []string
	mode  string
}

func (f *fakeDelegator) Delegate(dir, prompt, mode, label string) (string, error) {
	f.calls = append(f.calls, dir+"|"+prompt+"|"+label)
	f.mode = mode
	return "sess-1", nil
}

func (f *fakeDelegator) DelegateStatus(id string) (DelegateStatus, error) {
	if id != "sess-1" {
		return DelegateStatus{}, fs.ErrNotExist
	}
	return DelegateStatus{Session: id, State: "idle", Reply: "PONG"}, nil
}

type serviceEnv struct {
	srv   *httptest.Server
	store *Store
	audit *Audit
	root  string
	deleg *fakeDelegator
	token string
	id    string
}

func newServiceEnv(t *testing.T, p Policy) *serviceEnv {
	t.Helper()
	dir := t.TempDir()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "sub", "a.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1"), 0o644)
	os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0, 1, 2}, 0o644)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "x.txt"), []byte("outside"), 0o644)
	os.Symlink(outside, filepath.Join(root, "escape"))
	store, _ := NewStore(dir)
	audit, _ := NewAudit(dir, 100)
	c, token, err := store.AddClient("A", p)
	if err != nil {
		t.Fatal(err)
	}
	e := &serviceEnv{store: store, audit: audit, root: root, deleg: &fakeDelegator{}, token: token, id: c.ID}
	svc := &Service{Store: store, Audit: audit, Root: root, Version: "v1", Shell: "/bin/sh", Env: []string{"PATH=/usr/bin:/bin"},
		Status: func() any { return map[string]any{"cpus": 8} }, Delegator: e.deleg}
	e.srv = httptest.NewServer(svc.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

func (e *serviceEnv) do(t *testing.T, method, path, token string, body any) (int, map[string]any, string) {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var raw strings.Builder
	buf := make([]byte, 1<<16)
	for {
		n, err := res.Body.Read(buf)
		raw.Write(buf[:n])
		if err != nil {
			break
		}
	}
	var v map[string]any
	json.Unmarshal([]byte(raw.String()), &v)
	return res.StatusCode, v, raw.String()
}

var allowAll = Policy{Status: true, Files: true, Exec: true, Delegate: true, DelegateMode: "acceptEdits"}

func TestServiceAuth(t *testing.T) {
	e := newServiceEnv(t, allowAll)
	for _, tok := range []string{"", "wrong"} {
		if code, _, _ := e.do(t, "GET", "/peer/v1/hello", tok, nil); code != http.StatusUnauthorized {
			t.Errorf("token %q: %d", tok, code)
		}
	}
	code, v, _ := e.do(t, "GET", "/peer/v1/hello", e.token, nil)
	if code != 200 || v["client"] != "A" || v["version"] != "v1" || v["root"] != e.root {
		t.Fatalf("hello: %d %v", code, v)
	}
	// 削除した接続元のトークンは即座に使えない
	e.store.DeleteClient(e.id)
	if code, _, _ := e.do(t, "GET", "/peer/v1/hello", e.token, nil); code != http.StatusUnauthorized {
		t.Fatalf("deleted client: %d", code)
	}
}

func TestServicePolicyIsEnforced(t *testing.T) {
	e := newServiceEnv(t, Policy{})
	cases := []struct{ method, path string }{
		{"GET", "/peer/v1/status"},
		{"GET", "/peer/v1/files"},
		{"GET", "/peer/v1/file?path=sub/a.txt"},
		{"POST", "/peer/v1/exec"},
		{"POST", "/peer/v1/delegate"},
		{"GET", "/peer/v1/delegate/sess-1"},
	}
	for _, c := range cases {
		if code, _, _ := e.do(t, c.method, c.path, e.token, map[string]string{"command": "touch x", "prompt": "hi"}); code != http.StatusForbidden {
			t.Errorf("%s %s: %d", c.method, c.path, code)
		}
	}
	if _, err := os.Stat(filepath.Join(e.root, "x")); err == nil {
		t.Fatal("command ran although exec is not allowed")
	}
	if len(e.deleg.calls) != 0 {
		t.Fatal("delegated although not allowed")
	}
	// 拒否も記録に残る
	entries, _ := e.audit.Recent(100)
	if len(entries) != len(cases) || entries[0].OK || entries[0].Error == "" || entries[0].Client != "A" {
		t.Fatalf("audit = %+v", entries)
	}
	// 1つの操作だけ許可したとき、ほかの操作は断る（操作ごとに正しい許可を見ている）
	ops := []struct {
		policy Policy
		method string
		path   string
	}{
		{Policy{Status: true}, "GET", "/peer/v1/status"},
		{Policy{Files: true}, "GET", "/peer/v1/files"},
		{Policy{Files: true}, "GET", "/peer/v1/file?path=sub/a.txt"},
		{Policy{Exec: true}, "POST", "/peer/v1/exec"},
		{Policy{Delegate: true}, "POST", "/peer/v1/delegate"},
	}
	for _, only := range ops {
		e.store.SetPolicy(e.id, only.policy)
		for _, op := range ops {
			code, _, _ := e.do(t, op.method, op.path, e.token, map[string]string{"command": "true", "prompt": "hi"})
			allowed := op.policy == only.policy
			if allowed && code == http.StatusForbidden || !allowed && code != http.StatusForbidden {
				t.Errorf("policy %+v: %s %s -> %d", only.policy, op.method, op.path, code)
			}
		}
	}
	// 許可を変えれば、次の呼び出しから反映される
	e.store.SetPolicy(e.id, Policy{Status: true})
	if code, v, _ := e.do(t, "GET", "/peer/v1/status", e.token, nil); code != 200 || v["cpus"] != float64(8) {
		t.Fatalf("status after allow: %d %v", code, v)
	}
}

func TestServiceFiles(t *testing.T) {
	e := newServiceEnv(t, allowAll)
	code, v, _ := e.do(t, "GET", "/peer/v1/files", e.token, nil)
	if code != 200 || v["path"] != e.root {
		t.Fatalf("list root: %d %v", code, v)
	}
	// 隠しファイルも一覧に出す
	if names := entryNames(v); !strings.Contains(names, ".env") || !strings.Contains(names, "sub") {
		t.Fatalf("entries = %s", names)
	}
	// 相対パスはルートからとして扱う
	if code, v, _ := e.do(t, "GET", "/peer/v1/files?path=sub", e.token, nil); code != 200 || v["path"] != filepath.Join(e.root, "sub") {
		t.Fatalf("list relative: %d %v", code, v)
	}
	code, v, _ = e.do(t, "GET", "/peer/v1/file?path=sub/a.txt", e.token, nil)
	if code != 200 || v["content"] != "hello" {
		t.Fatalf("read: %d %v", code, v)
	}
	abs := filepath.Join(e.root, "sub", "a.txt")
	if code, v, _ := e.do(t, "GET", "/peer/v1/file?path="+abs, e.token, nil); code != 200 || v["content"] != "hello" {
		t.Fatalf("read absolute: %d %v", code, v)
	}
	for path, want := range map[string]int{
		"../x":           http.StatusForbidden, // ルートの外
		"escape/x.txt":   http.StatusForbidden, // シンボリックリンクで外へ
		"/etc/hostname":  http.StatusForbidden,
		"missing.txt":    http.StatusNotFound,
		"bin.dat":        http.StatusBadRequest, // バイナリ
		"sub":            http.StatusBadRequest, // フォルダ
		"sub/../../../x": http.StatusForbidden,
		// ルートの外は、存在しなくても404ではなく403（外のファイルの有無を知られない）
		"../definitely-missing":  http.StatusForbidden,
		"/nonexistent/dir/x.txt": http.StatusForbidden,
	} {
		if code, _, body := e.do(t, "GET", "/peer/v1/file?path="+path, e.token, nil); code != want {
			t.Errorf("read %q: %d (want %d) %s", path, code, want, body)
		}
	}
	entries, _ := e.audit.Recent(100)
	found := false
	for _, x := range entries {
		found = found || (x.Op == "read" && !x.OK && x.Detail == "sub/../../../x" && x.Error != "")
	}
	if !found {
		t.Fatalf("rejected read not audited: %+v", entries)
	}
}

func entryNames(v map[string]any) string {
	var names []string
	list, _ := v["entries"].([]any)
	for _, x := range list {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	return strings.Join(names, ",")
}

func TestServiceExec(t *testing.T) {
	e := newServiceEnv(t, allowAll)
	code, v, _ := e.do(t, "POST", "/peer/v1/exec", e.token, map[string]any{"command": "pwd; echo err >&2; exit 3", "cwd": "sub"})
	if code != 200 || v["exitCode"] != float64(3) || v["output"] != filepath.Join(e.root, "sub")+"\nerr\n" {
		t.Fatalf("exec: %d %v", code, v)
	}
	// cwdを省略するとルート
	if _, v, _ := e.do(t, "POST", "/peer/v1/exec", e.token, map[string]any{"command": "pwd"}); v["output"] != e.root+"\n" {
		t.Fatalf("default cwd: %v", v)
	}
	for _, body := range []map[string]any{
		{"command": "  "},
		{"command": strings.Repeat("x", maxCommand+1)},
		{"command": "pwd", "cwd": "../"},
		{"command": "pwd", "cwd": "sub/a.txt"},
	} {
		if code, _, _ := e.do(t, "POST", "/peer/v1/exec", e.token, body); code/100 != 4 {
			t.Errorf("exec %v: %d", body, code)
		}
	}
	// サーバの環境変数は渡さない（Envで指定したものだけ）
	if _, v, _ := e.do(t, "POST", "/peer/v1/exec", e.token, map[string]any{"command": "echo \"[$HOME]\""}); v["output"] != "[]\n" {
		t.Fatalf("env leaked: %v", v)
	}
	entries, _ := e.audit.Recent(10)
	found := false
	for _, x := range entries {
		found = found || (x.Op == "exec" && x.OK && strings.Contains(x.Detail, "exit 3"))
	}
	if !found {
		t.Fatalf("exec not audited: %+v", entries)
	}
}

func TestServiceDelegate(t *testing.T) {
	e := newServiceEnv(t, allowAll)
	code, v, _ := e.do(t, "POST", "/peer/v1/delegate", e.token, map[string]any{"prompt": " do it ", "cwd": "sub"})
	if code != http.StatusCreated || v["session"] != "sess-1" || v["permissionMode"] != "acceptEdits" {
		t.Fatalf("delegate: %d %v", code, v)
	}
	if len(e.deleg.calls) != 1 || e.deleg.calls[0] != filepath.Join(e.root, "sub")+"|do it|依頼: A" || e.deleg.mode != "acceptEdits" {
		t.Fatalf("delegator calls = %v mode=%q", e.deleg.calls, e.deleg.mode)
	}
	code, v, _ = e.do(t, "GET", "/peer/v1/delegate/sess-1", e.token, nil)
	if code != 200 || v["reply"] != "PONG" || v["state"] != "idle" {
		t.Fatalf("status: %d %v", code, v)
	}
	// 自分が依頼していないセッションは読めない
	if code, _, _ := e.do(t, "GET", "/peer/v1/delegate/other", e.token, nil); code != http.StatusNotFound {
		t.Fatalf("other session: %d", code)
	}
	_, token2, _ := e.store.AddClient("C", allowAll)
	if code, _, _ := e.do(t, "GET", "/peer/v1/delegate/sess-1", token2, nil); code != http.StatusNotFound {
		t.Fatalf("another client's session: %d", code)
	}
	for _, body := range []map[string]any{{"prompt": ""}, {"prompt": strings.Repeat("x", maxPrompt+1)}, {"prompt": "x", "cwd": "../.."}} {
		if code, _, _ := e.do(t, "POST", "/peer/v1/delegate", e.token, body); code/100 != 4 {
			t.Errorf("delegate %v: %d", body, code)
		}
	}
	if len(e.deleg.calls) != 1 {
		t.Fatalf("invalid requests reached the delegator: %v", e.deleg.calls)
	}
}

func TestServiceBodyLimit(t *testing.T) {
	e := newServiceEnv(t, allowAll)
	big := map[string]any{"command": "true", "pad": strings.Repeat("x", maxBodyBytes)}
	if code, _, _ := e.do(t, "POST", "/peer/v1/exec", e.token, big); code/100 != 4 {
		t.Fatalf("large body: %d", code)
	}
}

func TestRunCommand(t *testing.T) {
	dir := t.TempDir()
	res, err := runCommand(context.Background(), "/bin/sh", dir, "yes | head -c 5000", nil, 5*time.Second, 1000)
	if err != nil || !res.Truncated || len(res.Output) != 1000 || res.ExitCode != 0 {
		t.Fatalf("truncate: %+v %v", res, err)
	}
	start := time.Now()
	res, err = runCommand(context.Background(), "/bin/sh", dir, "sleep 5", nil, 200*time.Millisecond, 1000)
	if err != nil || !res.TimedOut || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout: %+v %v after %v", res, err, time.Since(start))
	}
	// バックグラウンドに残したプロセスも片付ける
	marker := filepath.Join(dir, "late")
	res, err = runCommand(context.Background(), "/bin/sh", dir, "(sleep 1; touch "+marker+") & echo started", nil, 5*time.Second, 1000)
	if err != nil || res.Output != "started\n" {
		t.Fatalf("background: %+v %v", res, err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("background process survived")
	}
	if _, err := runCommand(context.Background(), "/nonexistent/shell", dir, "true", nil, time.Second, 10); err == nil {
		t.Fatal("missing shell accepted")
	}
}

func TestCappedBufferReportsFullWrites(t *testing.T) {
	b := &cappedBuffer{max: 3}
	n, err := b.Write([]byte("hello"))
	if n != 5 || err != nil || string(b.buf) != "hel" || !b.truncated {
		t.Fatalf("n=%d err=%v buf=%q truncated=%v", n, err, b.buf, b.truncated)
	}
	if n, _ := b.Write([]byte("xy")); n != 2 || string(b.buf) != "hel" {
		t.Fatalf("after full: n=%d buf=%q", n, b.buf)
	}
}

func TestAudit(t *testing.T) {
	dir := t.TempDir()
	a, err := NewAudit(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		a.Add(AuditEntry{Op: "exec", Detail: strings.Repeat("d", i+1), OK: true})
	}
	got, _ := a.Recent(100)
	// 上限を超えたら半分まで減らしてから追記を続ける
	// 11件目で上限を超えて5件に減らし、12件目を足して6件
	if len(got) != 6 || got[0].Detail != strings.Repeat("d", 12) {
		t.Fatalf("len=%d first=%q", len(got), got[0].Detail)
	}
	if got2, _ := a.Recent(2); len(got2) != 2 {
		t.Fatalf("recent 2 = %d", len(got2))
	}
	a.Add(AuditEntry{Op: "exec", Detail: strings.Repeat("あ", maxAuditDetail+10)})
	last, _ := a.Recent(1)
	if r := []rune(last[0].Detail); len(r) != maxAuditDetail+1 || r[len(r)-1] != '…' {
		t.Fatalf("long detail not shortened: %d", len(r))
	}
	info, _ := os.Stat(filepath.Join(dir, "peer-audit.jsonl"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v", info.Mode().Perm())
	}
	// 壊れた行は飛ばして読み直せる
	f, _ := os.OpenFile(filepath.Join(dir, "peer-audit.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("{broken\n")
	f.Close()
	re, err := NewAudit(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := re.Recent(100); len(got) != 7 {
		t.Fatalf("reloaded len = %d", len(got))
	}
	if _, err := NewAudit(filepath.Join(dir, "missing", "x"), 10); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing dir: %v", err)
	}
}
