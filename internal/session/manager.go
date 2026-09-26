package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned for unknown session ids.
var ErrNotFound = errors.New("session not found")

// CommandFunc builds the process for a session. Tests substitute fakes.
type CommandFunc func(Spec) *exec.Cmd

// Event describes a change broadcast to UI clients.
type Event struct {
	Type    string `json:"type"`
	Session string `json:"session"`
}

// Manager owns all sessions and their persistence.
type Manager struct {
	mu          sync.Mutex
	sessions    map[string]*Session
	idleTimers  map[string]*time.Timer
	path        string
	command     CommandFunc
	idleTimeout time.Duration
	// OnEvent is called (outside locks) when session state changes.
	OnEvent func(Event)
}

// Options configures a new session.
type Options struct {
	Kind           string
	Cwd            string
	Model          string
	PermissionMode string
	Label          string
}

// NewManager loads persisted sessions from dataDir. Loaded sessions start stopped
// and are resumed lazily on first attach.
func NewManager(dataDir string, command CommandFunc, idleTimeout time.Duration) (*Manager, error) {
	m := &Manager{
		sessions:    map[string]*Session{},
		idleTimers:  map[string]*time.Timer{},
		path:        filepath.Join(dataDir, "sessions.json"),
		command:     command,
		idleTimeout: idleTimeout,
	}
	b, err := os.ReadFile(m.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(b) > 0 {
		var specs []Spec
		if err := json.Unmarshal(b, &specs); err != nil {
			return nil, fmt.Errorf("parse %s: %w", m.path, err)
		}
		for _, sp := range specs {
			m.sessions[sp.ID] = m.wire(newSession(sp))
		}
	}
	return m, nil
}

func (m *Manager) wire(s *Session) *Session {
	s.onClientsChanged = m.clientsChanged
	s.onExit = func(s *Session) {
		m.emit(Event{Type: "session.exited", Session: s.Spec().ID})
	}
	return s
}

func (m *Manager) emit(e Event) {
	if m.OnEvent != nil {
		m.OnEvent(e)
	}
}

// Create starts a new session.
func (m *Manager) Create(o Options) (*Session, error) {
	info, err := os.Stat(o.Cwd)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("cwd is not a directory: %s", o.Cwd)
	}
	if o.PermissionMode != "" && !slices.Contains(PermissionModes, o.PermissionMode) {
		return nil, fmt.Errorf("unknown permission mode: %s", o.PermissionMode)
	}
	switch o.Kind {
	case "", KindClaude:
		o.Kind = KindClaude
	case KindShell:
		// シェルにはモデルや権限モードの概念がないので、誤って保存しないよう捨てる
		o.Model, o.PermissionMode = "", ""
	default:
		return nil, fmt.Errorf("unknown session kind: %s", o.Kind)
	}
	claudeID := ""
	if o.Kind == KindClaude {
		claudeID = newUUID()
	}
	now := time.Now()
	s := m.wire(newSession(Spec{
		ID:              randHex(8),
		Kind:            o.Kind,
		ClaudeSessionID: claudeID,
		Cwd:             o.Cwd,
		Model:           o.Model,
		PermissionMode:  o.PermissionMode,
		Label:           o.Label,
		CreatedAt:       now,
		LastActiveAt:    now,
		HookKey:         randHex(16),
	}))
	if err := s.start(m.command(s.Spec())); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.sessions[s.spec.ID] = s
	m.mu.Unlock()
	m.save()
	m.clientsChanged(s, 0)
	m.emit(Event{Type: "session.created", Session: s.spec.ID})
	return s, nil
}

// Get returns a session by id.
func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	return s, nil
}

// Ensure starts the session's process if it is not running (resuming the conversation).
func (m *Manager) Ensure(id string) (*Session, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	if s.Running() {
		return s, nil
	}
	// 同時に複数クライアントから再開要求が来た場合、負けた側のstartは"already running"で失敗するが問題ない
	if err := s.start(m.command(s.Spec())); err != nil {
		if s.Running() {
			return s, nil
		}
		return nil, err
	}
	// 予約プロンプト経由の再開などでクライアント0のまま起動した場合もアイドル停止の対象にする
	m.clientsChanged(s, s.Status().Clients)
	m.emit(Event{Type: "session.started", Session: id})
	return s, nil
}

// List returns all sessions sorted by creation time.
func (m *Manager) List() []Status {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	out := make([]Status, 0, len(all))
	for _, s := range all {
		out = append(out, s.Status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Update applies fn to the session's spec and persists the result.
func (m *Manager) Update(id string, fn func(*Spec)) error {
	s, err := m.Get(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	fn(&s.spec)
	s.mu.Unlock()
	m.save()
	m.emit(Event{Type: "session.updated", Session: id})
	return nil
}

// SetActivity records what the session is doing (from hooks), with an optional
// human-readable detail, and notifies listeners when it changes. It is ignored
// for stopped sessions.
func (m *Manager) SetActivity(id, activity, detail string) error {
	s, err := m.Get(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	changed := s.running && (s.activity != activity || s.activityDetail != detail)
	if changed {
		s.activity, s.activityAt, s.activityDetail = activity, time.Now(), detail
	}
	s.mu.Unlock()
	if changed {
		m.emit(Event{Type: "session.updated", Session: id})
	}
	return nil
}

// Stop kills the process but keeps the session for later resume.
func (m *Manager) Stop(id string) error {
	s, err := m.Get(id)
	if err != nil {
		return err
	}
	s.Stop()
	m.save()
	return nil
}

// Delete stops the session and forgets it.
func (m *Manager) Delete(id string) error {
	s, err := m.Get(id)
	if err != nil {
		return err
	}
	s.Stop()
	s.closeClients()
	m.mu.Lock()
	delete(m.sessions, id)
	if t := m.idleTimers[id]; t != nil {
		t.Stop()
		delete(m.idleTimers, id)
	}
	m.mu.Unlock()
	m.save()
	m.emit(Event{Type: "session.deleted", Session: id})
	return nil
}

// Shutdown stops all processes, keeping their records for resume on next start.
func (m *Manager) Shutdown() {
	m.save()
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range all {
		wg.Add(1)
		go func() { defer wg.Done(); s.Stop() }()
	}
	wg.Wait()
}

// clientsChanged arms or cancels the idle timer.
func (m *Manager) clientsChanged(s *Session, n int) {
	id := s.Spec().ID
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.idleTimers[id]; t != nil {
		t.Stop()
		delete(m.idleTimers, id)
	}
	if n == 0 && m.idleTimeout > 0 {
		m.idleTimers[id] = time.AfterFunc(m.idleTimeout, func() {
			// タイマー発火までに誰かが接続していたら止めない
			if s.Status().Clients == 0 && s.Running() {
				log.Printf("[session %s] idle timeout, stopping", id)
				s.Stop()
			}
		})
	}
	go m.emit(Event{Type: "session.clients", Session: id})
}

func (m *Manager) save() {
	m.mu.Lock()
	specs := make([]Spec, 0, len(m.sessions))
	for _, s := range m.sessions {
		specs = append(specs, s.Spec())
	}
	m.mu.Unlock()
	sort.Slice(specs, func(i, j int) bool { return specs[i].CreatedAt.Before(specs[j].CreatedAt) })
	b, _ := json.MarshalIndent(specs, "", "  ")
	if err := writeFileAtomic(m.path, b); err != nil {
		log.Printf("save sessions: %v", err)
	}
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
