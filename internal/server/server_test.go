package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"tether/internal/events"
	"tether/internal/guard"
	"tether/internal/listen"
	"tether/internal/schedule"
	"tether/internal/session"
	"tether/internal/usage"
)

const token = "tkn"

type env struct {
	srv  *httptest.Server
	root string
	m    *session.Manager
	// transcripts maps claude session ids to transcript files for the chat API.
	transcripts map[string]string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	data := t.TempDir()
	m, err := session.NewManager(data, func(sp session.Spec) *exec.Cmd {
		c := exec.Command("cat")
		c.Dir = sp.Cwd
		return c
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	hub := events.NewHub()
	m.OnEvent = func(e session.Event) { hub.Publish(e) }
	sched, _ := schedule.New(data, nil)
	e := &env{root: root, m: m, transcripts: map[string]string{}}
	s := New(Deps{
		Transcripts: func(id string) (string, bool) {
			p, ok := e.transcripts[id]
			return p, ok
		},
		Root: root, Token: token, Sessions: m, Hub: hub,
		Notifier:   &events.Notifier{Hub: hub, Sessions: m},
		Scheduler:  sched,
		Usage:      &usage.Client{CredentialsPath: "/nonexistent", TTL: time.Minute},
		Version:    "v1.2.3-test",
		Repository: "https://example.com/tether",
		Static: fstest.MapFS{
			"index.html":    {Data: []byte("<html>app</html>")},
			"assets/app.js": {Data: []byte("js")},
		},
	})
	e.srv = httptest.NewServer(s.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) do(t *testing.T, method, path string, body any, authed bool) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, r)
	if authed {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(guard.CSRFHeader, "1")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestAuthBoundaries(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api/sessions", "/api/fs/list", "/api/usage", "/ws/events", "/api/unknown"} {
		if res := e.do(t, "GET", p, nil, false); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without token: %d", p, res.StatusCode)
		}
	}
	if res := e.do(t, "GET", "/api/health", nil, false); res.StatusCode != http.StatusOK {
		t.Errorf("health: %d", res.StatusCode)
	}
	if res := e.do(t, "GET", "/", nil, false); res.StatusCode != http.StatusOK {
		t.Errorf("static: %d", res.StatusCode)
	}
	// hookはトークン不要だが、キーが無ければ拒否される
	if res := e.do(t, "POST", "/internal/hook?sid=x", map[string]string{}, false); res.StatusCode == http.StatusNoContent {
		t.Errorf("hook without key accepted")
	}
}

func TestStaticFallbackAndCache(t *testing.T) {
	e := newEnv(t)
	res := e.do(t, "GET", "/some/client/route", nil, false)
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(string(b), "app") || res.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("fallback: %d %q %q", res.StatusCode, b, res.Header.Get("Cache-Control"))
	}
	res = e.do(t, "GET", "/assets/app.js", nil, false)
	if !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset cache: %q", res.Header.Get("Cache-Control"))
	}
}

func TestCreateSessionOutsideRootForbidden(t *testing.T) {
	e := newEnv(t)
	res := e.do(t, "POST", "/api/sessions", map[string]string{"cwd": "/"}, true)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d", res.StatusCode)
	}
	if len(e.m.List()) != 0 {
		t.Fatal("session created outside root")
	}
}

func TestSessionLifecycleOverHTTPAndWebSocket(t *testing.T) {
	e := newEnv(t)
	res := e.do(t, "POST", "/api/sessions", map[string]string{"cwd": e.root, "label": " demo "}, true)
	if res.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("create: %d %s", res.StatusCode, b)
	}
	var st session.Status
	json.NewDecoder(res.Body).Decode(&st)
	if st.Label != "demo" || !st.Running || st.HookKey != "" {
		t.Fatalf("created = %+v", st)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws/sessions/" + st.ID + "?cols=90&rows=30&token=" + token
	dial := func() *websocket.Conn {
		c, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c1 := dial()
	defer c1.CloseNow()
	// 接続直後は replay → state（サイズ）→ 出力の順に届く
	for _, want := range []string{`"type":"replay"`, `"type":"state"`} {
		typ, b, err := c1.Read(ctx)
		if err != nil || typ != websocket.MessageText || !strings.Contains(string(b), want) {
			t.Fatalf("expected %s first, got %v %q %v", want, typ, b, err)
		}
	}
	c2 := dial()
	defer c2.CloseNow()

	c1.Write(ctx, websocket.MessageText, []byte(`{"type":"input","data":"ping-123\n"}`))
	for _, c := range []*websocket.Conn{c1, c2} {
		var got bytes.Buffer
		for !strings.Contains(got.String(), "ping-123") {
			typ, b, err := c.Read(ctx)
			if err != nil {
				t.Fatalf("read: %v (got %q)", err, got.String())
			}
			if typ == websocket.MessageBinary {
				got.Write(b)
			}
		}
	}

	// 出力が溜まった後に接続すると、replay → state（サイズ）→ リプレイ本体の順に届く。
	// 受け手はサイズを合わせてからリプレイを描くので、この順番が崩れると画面の再現がずれる
	c3 := dial()
	defer c3.CloseNow()
	var order []string
	for len(order) < 3 {
		typ, b, err := c3.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v (order so far %v)", err, order)
		}
		switch {
		case typ == websocket.MessageBinary:
			order = append(order, "binary")
		case strings.Contains(string(b), `"type":"replay"`):
			order = append(order, "replay")
		case strings.Contains(string(b), `"type":"state"`):
			order = append(order, "state")
		}
	}
	if strings.Join(order, ",") != "replay,state,binary" {
		t.Fatalf("message order = %v", order)
	}

	// 別オリジンからのWebSocket接続は拒否される
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example"}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin ws: err=%v resp=%v", err, resp)
	}

	if res := e.do(t, "PATCH", "/api/sessions/"+st.ID, map[string]string{"label": "renamed"}, true); res.StatusCode != http.StatusNoContent {
		t.Fatalf("rename: %d", res.StatusCode)
	}
	if res := e.do(t, "POST", "/api/sessions/"+st.ID+"/stop", nil, true); res.StatusCode != http.StatusNoContent {
		t.Fatalf("stop: %d", res.StatusCode)
	}
	var list []session.Status
	json.NewDecoder(e.do(t, "GET", "/api/sessions", nil, true).Body).Decode(&list)
	if len(list) != 1 || list[0].Running || list[0].Label != "renamed" {
		t.Fatalf("list after stop = %+v", list)
	}
	if res := e.do(t, "DELETE", "/api/sessions/"+st.ID, nil, true); res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", res.StatusCode)
	}
	if res := e.do(t, "DELETE", "/api/sessions/"+st.ID, nil, true); res.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete: %d", res.StatusCode)
	}
}

func TestScheduleAPI(t *testing.T) {
	e := newEnv(t)
	if res := e.do(t, "POST", "/api/schedules", map[string]any{"sessionId": "nope", "prompt": "x", "runAt": time.Now()}, true); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: %d", res.StatusCode)
	}
	s, _ := e.m.Create(session.Options{Cwd: e.root})
	res := e.do(t, "POST", "/api/schedules", map[string]any{"sessionId": s.Spec().ID, "prompt": "hello", "runAt": time.Now().Add(time.Hour)}, true)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", res.StatusCode)
	}
	var it schedule.Item
	json.NewDecoder(res.Body).Decode(&it)
	if res := e.do(t, "DELETE", "/api/schedules/"+it.ID, nil, true); res.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", res.StatusCode)
	}
}

func TestCrossOriginWebSocketDoesNotResumeSession(t *testing.T) {
	e := newEnv(t)
	s, err := e.m.Create(session.Options{Cwd: e.root})
	if err != nil {
		t.Fatal(err)
	}
	e.m.Stop(s.Spec().ID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws/sessions/" + s.Spec().ID + "?token=" + token
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example"}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin ws: err=%v resp=%v", err, resp)
	}
	// 別オリジンからの接続で、停止中のセッションが起動されてはならない
	if s.Running() {
		t.Fatal("cross-origin websocket resumed the session")
	}
}

func TestGuardIsWired(t *testing.T) {
	e := newEnv(t)
	// 認証は通っていても、X-Tetherヘッダのない状態変更は拒否される
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/sessions", strings.NewReader(`{"cwd":"`+e.root+`"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden || len(e.m.List()) != 0 {
		t.Fatalf("POST without CSRF header: %d, sessions=%d", res.StatusCode, len(e.m.List()))
	}
	// 許可されていないHost（DNSリバインディング）は、静的ファイルも含めて拒否される
	req, _ = http.NewRequest("GET", e.srv.URL+"/", nil)
	req.Host = "rebind.evil.example"
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("rebinding host: %d", res.StatusCode)
	}
}

func TestShellSessionAPI(t *testing.T) {
	e := newEnv(t)
	res := e.do(t, "POST", "/api/sessions", map[string]string{"kind": "shell", "cwd": e.root}, true)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create shell: %d", res.StatusCode)
	}
	var st session.Status
	json.NewDecoder(res.Body).Decode(&st)
	if st.Kind != session.KindShell {
		t.Fatalf("kind = %q", st.Kind)
	}
	// シェルへの予約プロンプトは受け付けない
	res = e.do(t, "POST", "/api/schedules", map[string]any{"sessionId": st.ID, "prompt": "rm -rf ~", "runAt": time.Now().Add(time.Hour)}, true)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("schedule on shell: %d", res.StatusCode)
	}
	if res := e.do(t, "POST", "/api/sessions", map[string]string{"kind": "python", "cwd": e.root}, true); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown kind: %d", res.StatusCode)
	}
}

func TestHomeEndpoints(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api/notifications", "/api/system"} {
		if res := e.do(t, "GET", p, nil, false); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without token: %d", p, res.StatusCode)
		}
	}
	var notes []events.Notification
	res := e.do(t, "GET", "/api/notifications", nil, true)
	if err := json.NewDecoder(res.Body).Decode(&notes); err != nil || res.StatusCode != 200 {
		t.Fatalf("notifications: %d %v", res.StatusCode, err)
	}
	var sys struct {
		CPUs     int    `json:"cpus"`
		MemTotal uint64 `json:"memTotal"`
		DiskPath string `json:"diskPath"`
	}
	res = e.do(t, "GET", "/api/system", nil, true)
	if err := json.NewDecoder(res.Body).Decode(&sys); err != nil || sys.CPUs < 1 || sys.MemTotal == 0 || sys.DiskPath != e.root {
		t.Fatalf("system: %v %+v", err, sys)
	}
}

func TestAboutEndpoint(t *testing.T) {
	e := newEnv(t)
	if res := e.do(t, "GET", "/api/about", nil, false); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("about without token: %d", res.StatusCode)
	}
	var about struct {
		Version    string    `json:"version"`
		GoVersion  string    `json:"goVersion"`
		Platform   string    `json:"platform"`
		StartedAt  time.Time `json:"startedAt"`
		Repository string    `json:"repository"`
		License    string    `json:"license"`
	}
	res := e.do(t, "GET", "/api/about", nil, true)
	if err := json.NewDecoder(res.Body).Decode(&about); err != nil || res.StatusCode != 200 {
		t.Fatalf("about: %d %v", res.StatusCode, err)
	}
	want := runtime.GOOS + "/" + runtime.GOARCH
	if about.Version != "v1.2.3-test" || about.GoVersion != runtime.Version() || about.Platform != want ||
		about.Repository != "https://example.com/tether" || about.License != "MIT" {
		t.Errorf("about = %+v", about)
	}
	if about.StartedAt.IsZero() || time.Since(about.StartedAt) > time.Minute {
		t.Errorf("startedAt = %v", about.StartedAt)
	}
}

func TestAboutVersionDefaultsToDev(t *testing.T) {
	s := New(Deps{})
	rec := httptest.NewRecorder()
	s.about(rec, httptest.NewRequest("GET", "/api/about", nil))
	var about struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&about); err != nil || about.Version != "dev" {
		t.Errorf("version = %q (%v)", about.Version, err)
	}
}

func transcriptLine(uuid, role, content string) string {
	return `{"type":"` + role + `","uuid":"` + uuid + `","timestamp":"2026-09-26T03:00:00Z","message":{"role":"` + role + `","content":` + content + `}}` + "\n"
}

func TestTranscriptAPI(t *testing.T) {
	e := newEnv(t)
	s, _ := e.m.Create(session.Options{Cwd: e.root})
	id := s.Spec().ID
	get := func(offset int64) (items []map[string]any, next int64, truncated bool) {
		t.Helper()
		res := e.do(t, "GET", "/api/sessions/"+id+"/transcript?offset="+strconv.FormatInt(offset, 10), nil, true)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status %d", res.StatusCode)
		}
		var body struct {
			Items           []map[string]any
			Offset          int64
			Truncated       bool
			ClaudeSessionID string
		}
		json.NewDecoder(res.Body).Decode(&body)
		if body.ClaudeSessionID != s.Spec().ClaudeSessionID {
			t.Fatalf("claudeSessionId = %q", body.ClaudeSessionID)
		}
		return body.Items, body.Offset, body.Truncated
	}

	// 記録ファイルがまだない会話は空で返す
	if items, next, _ := get(0); len(items) != 0 || next != 0 {
		t.Fatalf("no transcript: %v %d", items, next)
	}

	path := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(path, []byte(transcriptLine("u1", "user", `"hello"`)), 0o644)
	e.transcripts[s.Spec().ClaudeSessionID] = path
	items, next, _ := get(0)
	if len(items) != 1 || items[0]["text"] != "hello" || next == 0 {
		t.Fatalf("first read: %v next=%d", items, next)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(transcriptLine("a1", "assistant", `[{"type":"text","text":"hi"}]`))
	f.Close()
	items, next2, _ := get(next)
	if len(items) != 1 || items[0]["text"] != "hi" || next2 <= next {
		t.Fatalf("incremental read: %v next=%d", items, next2)
	}

	// 初回は末尾だけ返す
	var b strings.Builder
	for i := 0; i < transcriptTail+20; i++ {
		b.WriteString(transcriptLine("m"+strconv.Itoa(i), "user", `"msg`+strconv.Itoa(i)+`"`))
	}
	os.WriteFile(path, []byte(b.String()), 0o644)
	items, _, truncated := get(0)
	if len(items) != transcriptTail || !truncated || items[len(items)-1]["text"] != "msg"+strconv.Itoa(transcriptTail+19) {
		t.Fatalf("tail: len=%d truncated=%v last=%v", len(items), truncated, items[len(items)-1]["text"])
	}

	sh, _ := e.m.Create(session.Options{Kind: session.KindShell, Cwd: e.root})
	if res := e.do(t, "GET", "/api/sessions/"+sh.Spec().ID+"/transcript", nil, true); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("shell transcript: %d", res.StatusCode)
	}
	if res := e.do(t, "GET", "/api/sessions/nope/transcript", nil, true); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session: %d", res.StatusCode)
	}
}

// waitOutput waits until the session's replay buffer contains want.
func waitOutput(t *testing.T, s *session.Session, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, replay := s.Attach(80, 24)
		s.Detach(c)
		if strings.Contains(string(replay), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	c, replay := s.Attach(80, 24)
	s.Detach(c)
	t.Fatalf("output %q not found in %q", want, replay)
}

func TestSessionInputAPI(t *testing.T) {
	e := newEnv(t)
	s, _ := e.m.Create(session.Options{Cwd: e.root})
	id := s.Spec().ID
	post := func(body map[string]string) int {
		t.Helper()
		return e.do(t, "POST", "/api/sessions/"+id+"/input", body, true).StatusCode
	}
	// catは入力をそのまま返すので、送った内容が端末に届いたかを出力で確かめる
	if c := post(map[string]string{"text": "hello-chat\n"}); c != http.StatusNoContent {
		t.Fatalf("text: %d", c)
	}
	// 端末のエコーで1回、Enterで行が確定してcatが出力して2回目。2回出ればEnterまで届いている
	waitOutput(t, s, "hello-chat")
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, replay := s.Attach(80, 24)
		s.Detach(c)
		if strings.Count(string(replay), "hello-chat") >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Enter was not sent after the text: %q", replay)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c := post(map[string]string{"key": "3"}); c != http.StatusNoContent {
		t.Fatalf("key: %d", c)
	}
	post(map[string]string{"key": "enter"})
	waitOutput(t, s, "3")

	for _, bad := range []map[string]string{{"key": "\x1b[2J"}, {"key": "ctrl-c"}, {"text": "  \n"}, {}} {
		if c := post(bad); c != http.StatusBadRequest {
			t.Errorf("%v: status %d, want 400", bad, c)
		}
	}

	// 複数キーは順番どおりに届く（catはそのままエコーするので、送った順に並ぶ）
	postKeys := func(keys []string) int {
		t.Helper()
		return e.do(t, "POST", "/api/sessions/"+id+"/input", map[string]any{"keys": keys}, true).StatusCode
	}
	if c := postKeys([]string{"7", "8", "9", "enter"}); c != http.StatusNoContent {
		t.Fatalf("keys: %d", c)
	}
	waitOutput(t, s, "789")
	tooMany := make([]string, maxInputKeys+1)
	for i := range tooMany {
		tooMany[i] = "down"
	}
	if c := postKeys(tooMany); c != http.StatusBadRequest {
		t.Fatalf("too many keys: %d", c)
	}
	// 1つでも許可されていないキーが混ざっていれば、何も送らない
	if c := postKeys([]string{"4", "\x03", "enter"}); c != http.StatusBadRequest {
		t.Fatalf("bad key in list: %d", c)
	}
	c, replay := s.Attach(80, 24)
	s.Detach(c)
	if strings.Contains(string(replay), "4") {
		t.Fatalf("keys were sent despite a rejected key: %q", replay)
	}
	if c := e.do(t, "POST", "/api/sessions/nope/input", map[string]string{"text": "x"}, true).StatusCode; c != http.StatusNotFound {
		t.Fatalf("unknown session: %d", c)
	}

	// 停止中のセッションは再開してから送る
	e.m.Stop(id)
	if c := post(map[string]string{"text": "after-resume"}); c != http.StatusNoContent || !s.Running() {
		t.Fatalf("resume: status %d running %v", c, s.Running())
	}
	waitOutput(t, s, "after-resume")
}

func TestContentSecurityPolicy(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/", "/api/health"} {
		res := e.do(t, "GET", path, nil, false)
		csp := res.Header.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "script-src 'self'", "img-src 'self' data: blob:", "frame-ancestors 'none'", "form-action 'none'", "object-src 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q lacks %q", path, csp, want)
			}
		}
		// 外部画像やスクリプトを許す指定が紛れ込んでいないこと
		for _, bad := range []string{"https:", "http:", "*", "'unsafe-eval'"} {
			if strings.Contains(csp, bad) {
				t.Errorf("%s: CSP %q allows %q", path, csp, bad)
			}
		}
		host := strings.TrimPrefix(e.srv.URL, "http://")
		if !strings.Contains(csp, "connect-src 'self' ws://"+host+" wss://"+host) {
			t.Errorf("%s: connect-src does not allow this host's websocket: %q", path, csp)
		}
	}
	// ダウンロードは、より厳しい専用のCSP（sandbox）で上書きされる
	os.WriteFile(filepath.Join(e.root, "f.png"), []byte("x"), 0o644)
	res := e.do(t, "GET", "/api/fs/download?inline=1&path="+filepath.Join(e.root, "f.png"), nil, true)
	if csp := res.Header.Get("Content-Security-Policy"); !strings.HasPrefix(csp, "sandbox") {
		t.Fatalf("download CSP = %q", csp)
	}
}

func TestCSPIgnoresHostWithSeparators(t *testing.T) {
	csp := contentSecurityPolicy("evil.example; script-src *")
	if strings.Contains(csp, "evil.example") || strings.Contains(csp, "script-src *") {
		t.Fatalf("host was injected into CSP: %q", csp)
	}
}

// tailscale serveがUnixソケットへ転送するときと同じ形（Host: localhost、X-Forwarded-Hostに元のホスト）で、
// POSTとWebSocketが通ることを本物のUnixソケット上で確かめる
func TestUnixSocketForwardedHost(t *testing.T) {
	e := newEnv(t)
	dir, err := os.MkdirTemp("/tmp", "tsv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := listen.Listen("unix:" + filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: e.srv.Config.Handler}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	sock := filepath.Join(dir, "s.sock")
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	const public = "devbox.example-tailnet.ts.net:3100"
	req, _ := http.NewRequest("POST", "http://localhost/api/sessions", strings.NewReader(`{"kind":"shell","cwd":"`+e.root+`"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(guard.CSRFHeader, "1")
	req.Header.Set("Origin", "https://"+public)
	req.Header.Set("X-Forwarded-Host", public)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	// ここではHost許可リストにtailscale名が入っていないので421になるのが正しい。
	// 許可されていれば（本番）通ることは、次のリクエストで確かめる
	if res.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("unlisted forwarded host: %d", res.StatusCode)
	}
	req2, _ := http.NewRequest("POST", "http://localhost/api/sessions", strings.NewReader(`{"kind":"shell","cwd":"`+e.root+`"}`))
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set(guard.CSRFHeader, "1")
	req2.Header.Set("Origin", "https://127.0.0.1:3100")
	req2.Header.Set("X-Forwarded-Host", "127.0.0.1:3100")
	res2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusCreated {
		t.Fatalf("POST via unix socket with forwarded host: %d", res2.StatusCode)
	}
	if csp := res2.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "wss://127.0.0.1:3100") {
		t.Fatalf("CSP does not use the forwarded host: %q", csp)
	}
	var st session.Status
	json.NewDecoder(res2.Body).Decode(&st)

	// WebSocketのOrigin検証も元のホスト名で行われる
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://localhost/ws/sessions/"+st.ID+"?token="+token, &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"Origin": {"https://127.0.0.1:3100"}, "X-Forwarded-Host": {"127.0.0.1:3100"}},
	})
	if err != nil {
		t.Fatalf("websocket via unix socket: %v", err)
	}
	c.CloseNow()
}
