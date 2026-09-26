// Package session runs Claude Code processes in ptys and multiplexes them
// to any number of attached clients.
package session

import (
	"bytes"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const (
	ringSize      = 2 << 20
	clientBacklog = 512
	defaultCols   = 120
	defaultRows   = 32
)

// Spec is the persisted description of a session.
type Spec struct {
	ID string `json:"id"`
	// Kind is KindClaude (also when empty) or KindShell.
	Kind            string    `json:"kind,omitempty"`
	ClaudeSessionID string    `json:"claudeSessionId"`
	Cwd             string    `json:"cwd"`
	Model           string    `json:"model,omitempty"`
	PermissionMode  string    `json:"permissionMode,omitempty"`
	Label           string    `json:"label,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	LastActiveAt    time.Time `json:"lastActiveAt"`
	// HookKey authenticates hook callbacks from this session's claude process.
	HookKey string `json:"hookKey"`
}

// Session kinds.
const (
	KindClaude = "claude"
	KindShell  = "shell"
)

// Activity states reported by Claude Code hooks.
const (
	ActivityWorking = "working"
	ActivityWaiting = "waiting"
	ActivityIdle    = "idle"
)

// IsShell reports whether the session runs a plain login shell.
func (sp Spec) IsShell() bool { return sp.Kind == KindShell }

// Frame is a message delivered to a client. Binary frames carry pty output;
// text frames carry JSON control messages.
type Frame struct {
	Binary bool
	Data   []byte
}

// Client is one attached viewer.
type Client struct {
	Out        chan Frame
	cols, rows int
	closed     bool
}

// Session is a (possibly stopped) Claude Code process.
type Session struct {
	mu      sync.Mutex
	spec    Spec
	cmd     *exec.Cmd
	ptmx    *os.File
	out     *ring
	clients map[*Client]struct{}
	running bool
	exit    int
	done    chan struct{}
	size    [2]int
	// activity is runtime-only state reported by Claude Code hooks.
	activity       string
	activityAt     time.Time
	activityDetail string
	// answeredAt is when input that answers a menu (Enter, Esc, a digit) last
	// arrived; toolStartAt is the last PreToolUse hook. Together they tell
	// whether a late permission notification was already answered.
	answeredAt  time.Time
	toolStartAt time.Time
	// Callbacks set by the manager.
	onClientsChanged func(s *Session, n int)
	onExit           func(s *Session)
	onActivity       func(s *Session)
}

func newSession(spec Spec) *Session {
	return &Session{spec: spec, clients: map[*Client]struct{}{}, out: newRing(ringSize)}
}

// DisplayLabel is the session's name for people: its label, or the folder name.
func DisplayLabel(sp Spec) string {
	if sp.Label != "" {
		return sp.Label
	}
	return filepath.Base(sp.Cwd)
}

// Spec returns a copy of the persisted description.
func (s *Session) Spec() Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spec
}

// Status is a snapshot for listings.
type Status struct {
	Spec
	// Activity is ActivityWorking, ActivityWaiting, ActivityIdle or "" (unknown).
	Activity   string    `json:"activity,omitempty"`
	ActivityAt time.Time `json:"activityAt,omitzero"`
	// ActivityDetail explains the activity, e.g. what permission is being asked.
	ActivityDetail string `json:"activityDetail,omitempty"`
	Running        bool   `json:"running"`
	Clients        int    `json:"clients"`
	ExitCode       int    `json:"exitCode"`
	Cols           int    `json:"cols"`
	Rows           int    `json:"rows"`
}

// Status returns a snapshot of runtime state.
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{
		Spec: s.spec, Activity: s.activity, ActivityAt: s.activityAt, ActivityDetail: s.activityDetail,
		Running: s.running, Clients: len(s.clients), ExitCode: s.exit, Cols: s.size[0], Rows: s.size[1],
	}
	st.HookKey = ""
	return st
}

// Running reports whether the process is alive.
func (s *Session) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// start launches cmd in a new pty. The caller must ensure the session is stopped.
func (s *Session) start(cmd *exec.Cmd) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return errors.New("session already running")
	}
	cols, rows := s.minSizeLocked()
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return err
	}
	s.cmd, s.ptmx, s.running, s.exit = cmd, ptmx, true, 0
	s.size = [2]int{cols, rows}
	s.done = make(chan struct{})
	// 再起動時は前回の出力を残さない（別プロセスの画面と混ざると表示が崩れるため）
	s.out = newRing(ringSize)
	s.broadcastLocked(Frame{Data: []byte(`{"type":"started"}`)})
	go s.readLoop(ptmx, cmd, s.done)
	return nil
}

func (s *Session) readLoop(ptmx *os.File, cmd *exec.Cmd, done chan struct{}) {
	buf := make([]byte, 32<<10)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			s.mu.Lock()
			s.out.Write(data)
			s.broadcastLocked(Frame{Binary: true, Data: data})
			s.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
	waitErr := cmd.Wait()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		code = exitErr.ExitCode()
	} else if waitErr != nil {
		code = -1
	}
	s.mu.Lock()
	// Setsize等はロック下でs.ptmxのFd()を使うため、Closeもロック下でnil化と同時に行う
	ptmx.Close()
	s.running, s.exit, s.cmd, s.ptmx = false, code, nil, nil
	s.activity, s.activityAt, s.activityDetail = "", time.Time{}, ""
	s.broadcastLocked(Frame{Data: []byte(`{"type":"exit","code":` + itoa(code) + `}`)})
	onExit := s.onExit
	s.mu.Unlock()
	close(done)
	if onExit != nil {
		onExit(s)
	}
}

// broadcastLocked sends f to every client. Clients that cannot keep up are
// disconnected rather than blocking the pty reader.
func (s *Session) broadcastLocked(f Frame) {
	for c := range s.clients {
		select {
		case c.Out <- f:
		default:
			s.detachLocked(c)
		}
	}
}

// Attach registers a client and returns the buffered output to replay.
// Registration and snapshot happen atomically so no output is lost or duplicated.
func (s *Session) Attach(cols, rows int) (*Client, []byte) {
	c := &Client{Out: make(chan Frame, clientBacklog), cols: cols, rows: rows}
	s.mu.Lock()
	s.clients[c] = struct{}{}
	replay := s.out.Bytes()
	s.resizeLocked()
	n := len(s.clients)
	cb := s.onClientsChanged
	s.mu.Unlock()
	if cb != nil {
		cb(s, n)
	}
	return c, replay
}

// Detach unregisters a client.
func (s *Session) Detach(c *Client) {
	s.mu.Lock()
	s.detachLocked(c)
	n := len(s.clients)
	cb := s.onClientsChanged
	s.mu.Unlock()
	if cb != nil {
		cb(s, n)
	}
}

func (s *Session) detachLocked(c *Client) {
	if _, ok := s.clients[c]; !ok {
		return
	}
	delete(s.clients, c)
	if !c.closed {
		c.closed = true
		close(c.Out)
	}
	s.resizeLocked()
}

// Resize records a client's viewport; the pty uses the smallest viewport.
func (s *Session) Resize(c *Client, cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clients[c]; !ok {
		return
	}
	c.cols, c.rows = cols, rows
	s.resizeLocked()
}

func (s *Session) minSizeLocked() (int, int) {
	cols, rows := 0, 0
	for c := range s.clients {
		if c.cols <= 0 || c.rows <= 0 {
			continue
		}
		if cols == 0 || c.cols < cols {
			cols = c.cols
		}
		if rows == 0 || c.rows < rows {
			rows = c.rows
		}
	}
	if cols == 0 {
		cols, rows = defaultCols, defaultRows
	}
	return cols, rows
}

func (s *Session) resizeLocked() {
	cols, rows := s.minSizeLocked()
	if s.size == [2]int{cols, rows} {
		return
	}
	s.size = [2]int{cols, rows}
	if s.ptmx != nil {
		pty.Setsize(s.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	}
	s.broadcastLocked(Frame{Data: []byte(`{"type":"size","cols":` + itoa(cols) + `,"rows":` + itoa(rows) + `}`)})
}

// Write sends input to the pty. Input that answers a menu while the session
// is waiting for a choice marks it as working again.
func (s *Session) Write(p []byte) error {
	s.mu.Lock()
	ptmx := s.ptmx
	now := time.Now()
	s.spec.LastActiveAt = now
	changed := false
	if ptmx != nil && answersMenu(p) {
		s.answeredAt = now
		// 選択待ちの状態は次のhook（Stop等）まで変わらないので、答えた時点で作業中に戻す
		if s.activity == ActivityWaiting {
			s.activity, s.activityAt, s.activityDetail = ActivityWorking, now, ""
			changed = true
		}
	}
	onActivity := s.onActivity
	s.mu.Unlock()
	if ptmx == nil {
		return errors.New("session is not running")
	}
	if changed && onActivity != nil {
		onActivity(s)
	}
	_, err := ptmx.Write(p)
	return err
}

// answersMenu reports whether input confirms or cancels a Claude Code menu:
// Enter, a lone Esc, or a single digit that picks an option directly.
// Arrow keys (which start with Esc) only move the cursor.
func answersMenu(p []byte) bool {
	switch {
	case bytes.IndexByte(p, '\r') >= 0:
		return true
	case len(p) == 1 && (p[0] == 0x1b || ('1' <= p[0] && p[0] <= '9')):
		return true
	}
	return false
}

// Stop terminates the process group, escalating to SIGKILL after a grace period.
func (s *Session) Stop() {
	s.mu.Lock()
	cmd, done := s.cmd, s.done
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	// pty.StartはSetsidするので、pgid=pidのプロセスグループごと止めて子プロセスを残さない
	pgid := -cmd.Process.Pid
	syscall.Kill(pgid, syscall.SIGTERM)
	if waitDone(done, 3*time.Second) {
		return
	}
	// ジョブ制御などで別プロセスグループに移った孫プロセスがSIGHUPを無視してptyを開いたままだと、
	// readLoopがEOFを受け取れず永久に待つ。グループではなくセッション全体を止める
	syscall.Kill(pgid, syscall.SIGKILL)
	killSession(cmd.Process.Pid)
	if !waitDone(done, 5*time.Second) {
		log.Printf("[session %s] process did not exit after SIGKILL", s.Spec().ID)
	}
}

// killSession sends SIGKILL to every process whose session id is sid.
func killSession(sid int) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// 形式は "pid (comm) state ppid pgrp session ..."。commに空白や括弧が入り得るので最後の')'以降を読む
		i := bytes.LastIndexByte(b, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(b[i+1:]))
		if len(f) > 3 && f[3] == strconv.Itoa(sid) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

func waitDone(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// Done returns a channel closed when the current process exits.
func (s *Session) Done() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done == nil {
		c := make(chan struct{})
		close(c)
		return c
	}
	return s.done
}

// closeClients disconnects every client (used on delete).
func (s *Session) closeClients() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		s.detachLocked(c)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
