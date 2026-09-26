package session

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// fakeCommand runs a shell script instead of claude.
func fakeCommand(script string) CommandFunc {
	return func(sp Spec) *exec.Cmd {
		cmd := exec.Command("sh", "-c", script)
		cmd.Dir = sp.Cwd
		return cmd
	}
}

// readUntil collects binary frames from c until want appears or timeout.
func readUntil(t *testing.T, c *Client, want string) string {
	t.Helper()
	var got bytes.Buffer
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f, ok := <-c.Out:
			if !ok {
				t.Fatalf("client closed before %q; got %q", want, got.String())
			}
			if f.Binary {
				got.Write(f.Data)
			}
			if strings.Contains(got.String(), want) {
				return got.String()
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %q; got %q", want, got.String())
		}
	}
}

// attachAndRead attaches a client and returns output (replay included) up to want.
// Output produced before Attach lands in the replay buffer, so it must be included.
func attachAndRead(t *testing.T, s *Session, want string) (*Client, string) {
	t.Helper()
	c, replay := s.Attach(80, 24)
	if strings.Contains(string(replay), want) {
		return c, string(replay)
	}
	return c, string(replay) + readUntil(t, c, want)
}

// childPid extracts N from "child=N" in shell output.
func childPid(t *testing.T, out string) int {
	t.Helper()
	i := strings.Index(out, "child=")
	if i < 0 {
		t.Fatalf("no child pid in %q", out)
	}
	rest := out[i+len("child="):]
	end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(rest)
	}
	pid, err := strconv.Atoi(rest[:end])
	if err != nil || pid == 0 {
		t.Fatalf("bad child pid in %q", out)
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

// processGone waits up to d for pid to exit. A zombie (exited, not yet reaped
// by its new parent) counts as gone.
func processGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		b, err := os.ReadFile(filepath.Join("/proc", itoa(pid), "stat"))
		if err != nil {
			return true
		}
		// 形式は "pid (comm) state ..."。commに括弧が入り得るので最後の')'の後を見る
		if i := bytes.LastIndexByte(b, ')'); i >= 0 && i+2 < len(b) && b[i+2] == 'Z' {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func newTestManager(t *testing.T, script string, idle time.Duration) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	m, err := NewManager(dir, fakeCommand(script), idle)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	return m, dir
}

func TestSharedSessionAndReplay(t *testing.T) {
	m, dir := newTestManager(t, "cat", 0)
	s, err := m.Create(Options{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Attach(80, 24)
	if err := s.Write([]byte("hello-from-a\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, a, "hello-from-a")

	// 後から接続したクライアントには、それまでの出力がリプレイされる
	b, replay := s.Attach(80, 24)
	if !strings.Contains(string(replay), "hello-from-a") {
		t.Fatalf("replay = %q", replay)
	}
	// どちらのクライアントの入力も両方に届く
	s.Write([]byte("hello-from-b\n"))
	readUntil(t, a, "hello-from-b")
	readUntil(t, b, "hello-from-b")
}

func TestPtyUsesSmallestViewport(t *testing.T) {
	m, dir := newTestManager(t, "cat", 0)
	s, _ := m.Create(Options{Cwd: dir})
	a, _ := s.Attach(100, 40)
	b, _ := s.Attach(80, 50)

	size := func() (int, int) {
		s.mu.Lock()
		defer s.mu.Unlock()
		rows, cols, err := pty.Getsize(s.ptmx)
		if err != nil {
			t.Fatal(err)
		}
		return cols, rows
	}
	if c, r := size(); c != 80 || r != 40 {
		t.Fatalf("size = %dx%d, want 80x40", c, r)
	}
	s.Resize(a, 60, 45)
	if c, r := size(); c != 60 || r != 45 {
		t.Fatalf("after resize = %dx%d, want 60x45", c, r)
	}
	s.Detach(a)
	if c, r := size(); c != 80 || r != 50 {
		t.Fatalf("after detach = %dx%d, want 80x50", c, r)
	}
	_ = b
}

func TestPersistAndLazyResume(t *testing.T) {
	dir := t.TempDir()
	m1, err := NewManager(dir, fakeCommand("cat"), 0)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m1.Create(Options{Cwd: dir, Label: "work"})
	id, csid := s.Spec().ID, s.Spec().ClaudeSessionID
	m1.Shutdown()

	var launched []Spec
	m2, err := NewManager(dir, func(sp Spec) *exec.Cmd {
		launched = append(launched, sp)
		return exec.Command("cat")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m2.Shutdown)
	list := m2.List()
	if len(list) != 1 || list[0].Running || list[0].Label != "work" || list[0].HookKey != "" {
		t.Fatalf("list after restart = %+v", list)
	}
	s2, err := m2.Ensure(id)
	if err != nil || !s2.Running() {
		t.Fatalf("ensure: %v running=%v", err, s2 != nil && s2.Running())
	}
	if len(launched) != 1 || launched[0].ClaudeSessionID != csid {
		t.Fatalf("resumed with %+v, want claude session %s", launched, csid)
	}
	// 実行中なら二重起動しない
	m2.Ensure(id)
	if len(launched) != 1 {
		t.Fatalf("ensure started a second process")
	}
}

func TestIdleTimeoutStopsUnattendedSession(t *testing.T) {
	m, dir := newTestManager(t, "cat", 150*time.Millisecond)
	s, _ := m.Create(Options{Cwd: dir})
	c, _ := s.Attach(80, 24)
	time.Sleep(300 * time.Millisecond)
	if !s.Running() {
		t.Fatal("stopped while a client was attached")
	}
	s.Detach(c)
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("idle session was not stopped")
	}
	if _, err := m.Get(s.Spec().ID); err != nil {
		t.Fatal("idle stop must keep the session record")
	}
}

func TestStopTerminatesProcessGroupGracefully(t *testing.T) {
	// SIGTERMはリーダーだけでなくプロセスグループ全体に送る。子がSIGHUPを無視していても、
	// SIGKILLへのエスカレーション（3秒後）を待たずに全員が終了すること
	m, dir := newTestManager(t, "(trap '' HUP; exec sleep 1000) & echo child=$!; wait", 0)
	s, _ := m.Create(Options{Cwd: dir})
	_, out := attachAndRead(t, s, "\n")
	pid := childPid(t, out)
	start := time.Now()
	m.Stop(s.Spec().ID)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Stop took %v; SIGTERM did not reach the whole group", d)
	}
	if s.Running() {
		t.Fatal("still running after Stop")
	}
	if !processGone(pid, time.Second) {
		t.Fatalf("child %d survived Stop", pid)
	}
}

func TestStopDoesNotHangOnDetachedGrandchild(t *testing.T) {
	// ジョブ制御(set -m)で別プロセスグループに移り、SIGHUPも無視する孫がptyを掴み続けても、Stopは有限時間で返ること。
	// このプロセスにはグループ宛てのシグナルが届かず、ptyのEOFも来ないため、pty側を閉じないと永久に待つ
	m, dir := newTestManager(t, "set -m; (trap '' HUP TERM; exec sleep 1000) & echo child=$!; wait", 0)
	s, _ := m.Create(Options{Cwd: dir})
	_, out := attachAndRead(t, s, "\n")
	pid := childPid(t, out)
	start := time.Now()
	stopped := make(chan struct{})
	go func() {
		m.Stop(s.Spec().ID)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("Stop hung while an escaped grandchild held the pty")
	}
	t.Logf("Stop returned after %v", time.Since(start).Round(time.Millisecond))
	if s.Running() {
		t.Fatal("still running after Stop")
	}
	if !processGone(pid, time.Second) {
		t.Fatalf("detached grandchild %d survived Stop", pid)
	}
}

func TestDeleteForgetsSession(t *testing.T) {
	m, dir := newTestManager(t, "cat", 0)
	s, _ := m.Create(Options{Cwd: dir})
	c, _ := s.Attach(80, 24)
	m.Delete(s.Spec().ID)
	if _, err := m.Get(s.Spec().ID); err != ErrNotFound {
		t.Fatalf("get after delete: %v", err)
	}
	for range c.Out {
	}
	b, _ := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if strings.Contains(string(b), s.Spec().ID) {
		t.Fatal("deleted session still persisted")
	}
}

func TestCreateValidation(t *testing.T) {
	m, dir := newTestManager(t, "cat", 0)
	if _, err := m.Create(Options{Cwd: filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("missing cwd accepted")
	}
	if _, err := m.Create(Options{Cwd: dir, PermissionMode: "yolo"}); err == nil {
		t.Fatal("unknown permission mode accepted")
	}
}

func TestLauncherCommand(t *testing.T) {
	cfg := t.TempDir()
	l := &Launcher{
		Bin: "/usr/bin/claude", HookExe: "/opt/cc deck/tether", HookURL: "http://127.0.0.1:3100/internal/hook",
		ClaudeConfigDir: cfg,
		Env: []string{"PATH=/bin", "TETHER_TOKEN=secret", "CLAUDECODE=1", "CLAUDE_CODE_CHILD_SESSION=1",
			"CLAUDE_CODE_MESSAGING_TOKEN=x", "CLAUDE_CONFIG_DIR=/cfg", "TERM=dumb", "HOME=/h"},
	}
	sp := Spec{ID: "abc", ClaudeSessionID: "11111111-2222-4333-8444-555555555555", Cwd: "/tmp", Model: "opus", PermissionMode: "acceptEdits", HookKey: "k'ey"}

	cmd := l.Command(sp)
	args := cmd.Args[1:]
	if !slices.Contains(args, "--session-id") || slices.Contains(args, "--resume") {
		t.Fatalf("new session args = %v", args)
	}
	if i := slices.Index(args, "--permission-mode"); i < 0 || args[i+1] != "acceptEdits" {
		t.Fatalf("permission mode args = %v", args)
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "TETHER_") || strings.HasPrefix(kv, "CLAUDECODE=") || kv == "TERM=dumb" ||
			strings.HasPrefix(kv, "CLAUDE_CODE_CHILD_SESSION=") || strings.HasPrefix(kv, "CLAUDE_CODE_MESSAGING_TOKEN=") {
			t.Fatalf("leaked env %q", kv)
		}
	}
	// ユーザー設定であるCLAUDE_CONFIG_DIRは引き継ぐ
	if !slices.Contains(cmd.Env, "TERM=xterm-256color") || !slices.Contains(cmd.Env, "HOME=/h") || !slices.Contains(cmd.Env, "CLAUDE_CONFIG_DIR=/cfg") {
		t.Fatalf("env = %v", cmd.Env)
	}

	var settings struct {
		Hooks map[string][]hookMatcher
	}
	if err := json.Unmarshal([]byte(args[slices.Index(args, "--settings")+1]), &settings); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"Stop", "Notification", "SessionStart", "UserPromptSubmit", "PreToolUse"} {
		hs := settings.Hooks[ev]
		if len(hs) != 1 || !strings.Contains(hs[0].Hooks[0].Command, `'/opt/cc deck/tether' hook`) || !strings.Contains(hs[0].Hooks[0].Command, `'k'\''ey'`) {
			t.Fatalf("hook %s = %+v", ev, hs)
		}
	}

	// トランスクリプトが存在すれば--resumeになる
	os.MkdirAll(filepath.Join(cfg, "projects", "-tmp"), 0o755)
	os.WriteFile(filepath.Join(cfg, "projects", "-tmp", sp.ClaudeSessionID+".jsonl"), nil, 0o644)
	args = l.Command(sp).Args[1:]
	if i := slices.Index(args, "--resume"); i < 0 || args[i+1] != sp.ClaudeSessionID || slices.Contains(args, "--session-id") {
		t.Fatalf("resume args = %v", args)
	}
}

func TestLauncherShellCommand(t *testing.T) {
	l := &Launcher{
		Bin: "/usr/bin/claude", HookExe: "/opt/tether", HookURL: "http://127.0.0.1:3100/internal/hook",
		Env:   []string{"PATH=/bin", "TETHER_TOKEN=secret", "CLAUDE_CODE_CHILD_SESSION=1", "HOME=/h"},
		Shell: "/usr/bin/zsh",
	}
	cmd := l.Command(Spec{ID: "s1", Kind: KindShell, Cwd: "/srv/app", HookKey: "k"})
	if !slices.Equal(cmd.Args, []string{"/usr/bin/zsh", "-l"}) || cmd.Dir != "/srv/app" {
		t.Fatalf("shell command = %v in %q", cmd.Args, cmd.Dir)
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "TETHER_") || strings.HasPrefix(kv, "CLAUDE_CODE_CHILD_SESSION=") {
			t.Fatalf("leaked env %q", kv)
		}
	}
	if !slices.Contains(cmd.Env, "TERM=xterm-256color") {
		t.Fatalf("env = %v", cmd.Env)
	}
	l.Shell = ""
	if got := l.Command(Spec{Kind: KindShell, Cwd: "/"}).Args[0]; got != "/bin/bash" {
		t.Fatalf("fallback shell = %q", got)
	}
}

func TestCreateShellSession(t *testing.T) {
	var launched []Spec
	dir := t.TempDir()
	m, err := NewManager(dir, func(sp Spec) *exec.Cmd {
		launched = append(launched, sp)
		return exec.Command("cat")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	s, err := m.Create(Options{Kind: KindShell, Cwd: dir, Model: "opus", PermissionMode: "bypassPermissions"})
	if err != nil {
		t.Fatal(err)
	}
	sp := s.Spec()
	if !sp.IsShell() || sp.ClaudeSessionID != "" || sp.Model != "" || sp.PermissionMode != "" {
		t.Fatalf("shell spec = %+v", sp)
	}
	c, err := m.Create(Options{Cwd: dir})
	if err != nil || c.Spec().Kind != KindClaude || c.Spec().ClaudeSessionID == "" {
		t.Fatalf("default kind: %+v %v", c.Spec(), err)
	}
	if _, err := m.Create(Options{Kind: "python", Cwd: dir}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	// 種類は永続化され、再起動後の再開でもシェルとして起動される
	m.Shutdown()
	m2, err := NewManager(dir, func(sp Spec) *exec.Cmd {
		launched = append(launched, sp)
		return exec.Command("cat")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m2.Shutdown)
	if _, err := m2.Ensure(sp.ID); err != nil {
		t.Fatal(err)
	}
	if last := launched[len(launched)-1]; !last.IsShell() {
		t.Fatalf("resumed as %+v", last)
	}
}

func TestTranscriptPath(t *testing.T) {
	cfg := t.TempDir()
	os.MkdirAll(filepath.Join(cfg, "projects", "-home-u-app"), 0o755)
	want := filepath.Join(cfg, "projects", "-home-u-app", "abc-123.jsonl")
	os.WriteFile(want, nil, 0o644)
	l := &Launcher{ClaudeConfigDir: cfg}
	if got, ok := l.TranscriptPath("abc-123"); !ok || got != want {
		t.Fatalf("TranscriptPath = %q %v", got, ok)
	}
	// IDはパスの一部になるので、ワイルドカードやパス区切りでほかのファイルを指せないこと
	for _, bad := range []string{"", "*", "abc-*", "../-home-u-app/abc-123", "a/b", "abc-12?"} {
		if got, ok := l.TranscriptPath(bad); ok {
			t.Errorf("TranscriptPath(%q) = %q, want not found", bad, got)
		}
	}
}

func TestAnswersMenu(t *testing.T) {
	for in, want := range map[string]bool{
		"\r":     true,
		"abc\r":  true, // 入力欄の送信
		"\x1b":   true, // Escでキャンセル
		"1":      true,
		"9":      true,
		"0":      false,
		"12":     false, // 数字の入力（選択ではない）
		"\x1b[A": false, // ↑は移動だけ
		"\x1b[B": false,
		"\t":     false,
		"y":      false,
		"hello":  false,
		"":       false,
	} {
		if got := answersMenu([]byte(in)); got != want {
			t.Errorf("answersMenu(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestAnsweringClearsWaiting(t *testing.T) {
	m, dir := newTestManager(t, "cat", 0)
	var events []Event
	var mu sync.Mutex
	m.OnEvent = func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() }
	s, err := m.Create(Options{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	activity := func() string { return s.Status().Activity }
	updates := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, e := range events {
			if e.Type == "session.updated" && e.Session == s.Spec().ID {
				n++
			}
		}
		return n
	}

	m.SetActivity(s.Spec().ID, ActivityWaiting, "Claude needs your permission")
	before := updates()
	// カーソルの移動では答えたことにならない
	s.Write([]byte("\x1b[B"))
	if activity() != ActivityWaiting {
		t.Fatalf("arrow key changed activity to %q", activity())
	}
	s.Write([]byte("\r"))
	st := s.Status()
	if st.Activity != ActivityWorking || st.ActivityDetail != "" {
		t.Fatalf("after Enter: %q %q", st.Activity, st.ActivityDetail)
	}
	if updates() != before+1 {
		t.Fatalf("session.updated events: %d -> %d", before, updates())
	}
	// 待ちでないときのEnterは状態を変えない（完了のまま）
	m.SetActivity(s.Spec().ID, ActivityIdle, "")
	s.Write([]byte("\r"))
	if activity() != ActivityIdle {
		t.Fatalf("Enter while idle changed activity to %q", activity())
	}
}

func TestLateWaitingNotification(t *testing.T) {
	m, dir := newTestManager(t, "cat", 0)
	s, err := m.Create(Options{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	id := s.Spec().ID
	activity := func() string { return s.Status().Activity }

	// PreToolUseを受けていないセッションでは、従来どおり待ちにする
	s.Write([]byte("\r"))
	if set, _ := m.SetWaiting(id, "x"); !set || activity() != ActivityWaiting {
		t.Fatalf("without PreToolUse: set=%v activity=%q", set, activity())
	}
	m.SetActivity(id, ActivityWorking, "")

	// ツール開始 → 通知 の順なら待ちになる
	m.ToolStarted(id)
	if set, _ := m.SetWaiting(id, "x"); !set || activity() != ActivityWaiting {
		t.Fatalf("prompt not answered: set=%v activity=%q", set, activity())
	}
	// 待ちの間に次のツールが始まったら、もう答えている
	m.ToolStarted(id)
	if activity() != ActivityWorking {
		t.Fatalf("PreToolUse while waiting: %q", activity())
	}

	// ツール開始 → ターミナルで回答 → 遅れて通知 の順なら、待ちにしない
	m.ToolStarted(id)
	time.Sleep(2 * time.Millisecond)
	s.Write([]byte("1"))
	if set, _ := m.SetWaiting(id, "x"); set || activity() != ActivityWorking {
		t.Fatalf("already answered: set=%v activity=%q", set, activity())
	}
	// 次のツールの許可確認では、また待ちにできる
	time.Sleep(2 * time.Millisecond)
	m.ToolStarted(id)
	if set, _ := m.SetWaiting(id, "y"); !set || activity() != ActivityWaiting {
		t.Fatalf("next prompt: set=%v activity=%q", set, activity())
	}
}

func TestLauncherRemoteTools(t *testing.T) {
	remote := false
	l := &Launcher{
		Bin: "/usr/bin/claude", HookExe: "/opt/tether", HookURL: "unix:/run/t.sock",
		ClaudeConfigDir: t.TempDir(), Env: []string{"PATH=/bin"},
		RemoteTools: func() bool { return remote },
	}
	sp := Spec{ID: "abc", ClaudeSessionID: "11111111-2222-4333-8444-555555555555", Cwd: "/tmp", HookKey: "k1"}
	type settings struct {
		Permissions *struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	parse := func(args []string) settings {
		var s settings
		if err := json.Unmarshal([]byte(args[slices.Index(args, "--settings")+1]), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// 接続先がなければMCPサーバも許可も渡さない
	args := l.Command(sp).Args[1:]
	if slices.Contains(args, "--mcp-config") || parse(args).Permissions != nil {
		t.Fatalf("without remotes: %v", args)
	}

	remote = true
	args = l.Command(sp).Args[1:]
	i := slices.Index(args, "--mcp-config")
	if i < 0 {
		t.Fatalf("no --mcp-config: %v", args)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(args[i+1]), &cfg); err != nil {
		t.Fatal(err)
	}
	srv := cfg.MCPServers[MCPServerName]
	want := []string{"mcp", "--url", "unix:/run/t.sock", "--sid", "abc", "--key", "k1"}
	if srv.Type != "stdio" || srv.Command != "/opt/tether" || !slices.Equal(srv.Args, want) {
		t.Fatalf("mcp server = %+v", srv)
	}
	// tetherの画面で承認するので、Claude Code側の確認は出さない
	if p := parse(args).Permissions; p == nil || !slices.Equal(p.Allow, []string{"mcp__tether-remote"}) {
		t.Fatalf("permissions = %+v", p)
	}
	// シェルのセッションには関係ない
	sh := l.Command(Spec{ID: "s", Kind: KindShell, Cwd: "/tmp"}).Args
	if slices.Contains(sh, "--mcp-config") {
		t.Fatalf("shell args = %v", sh)
	}
}

func TestCleanEnv(t *testing.T) {
	got := CleanEnv([]string{"PATH=/bin", "TETHER_TOKEN=x", "CLAUDECODE=1", "HOME=/h", "TERM=dumb"})
	if !slices.Equal(got, []string{"PATH=/bin", "HOME=/h", "TERM=dumb"}) {
		t.Fatalf("CleanEnv = %v", got)
	}
}
