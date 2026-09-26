// Package schedule sends prompts to sessions at a given time.
package schedule

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Status values of a scheduled prompt.
const (
	Pending = "pending"
	Done    = "done"
	Failed  = "failed"
)

// Item is one scheduled prompt.
type Item struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId"`
	Prompt    string    `json:"prompt"`
	RunAt     time.Time `json:"runAt"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// Target is the session side the scheduler talks to.
type Target interface {
	// Ensure starts the session if needed and reports whether it was just started.
	Ensure(id string) (started bool, err error)
	// Write sends raw input to the session.
	Write(id string, p []byte) error
}

// Scheduler persists items and fires them when due.
type Scheduler struct {
	mu     sync.Mutex
	items  map[string]*Item
	path   string
	target Target
	// StartupDelay is how long to wait after resuming a stopped session.
	StartupDelay time.Duration
	// EnterDelay separates the prompt text from the Enter key.
	EnterDelay time.Duration
	// OnFire is called after each attempt.
	OnFire func(Item)
	now    func() time.Time
}

// New loads items from dataDir.
func New(dataDir string, target Target) (*Scheduler, error) {
	s := &Scheduler{
		items: map[string]*Item{}, path: filepath.Join(dataDir, "schedules.json"), target: target,
		StartupDelay: 8 * time.Second, EnterDelay: 300 * time.Millisecond, now: time.Now,
	}
	b, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(b) > 0 {
		var items []*Item
		if err := json.Unmarshal(b, &items); err != nil {
			return nil, fmt.Errorf("parse %s: %w", s.path, err)
		}
		for _, it := range items {
			s.items[it.ID] = it
		}
	}
	return s, nil
}

// Add validates and stores a new pending item.
func (s *Scheduler) Add(sessionID, prompt string, runAt time.Time) (Item, error) {
	prompt = strings.TrimRight(prompt, "\r\n")
	if strings.TrimSpace(prompt) == "" {
		return Item{}, errors.New("prompt is empty")
	}
	if runAt.IsZero() {
		return Item{}, errors.New("runAt is required")
	}
	b := make([]byte, 8)
	rand.Read(b)
	it := &Item{ID: hex.EncodeToString(b), SessionID: sessionID, Prompt: prompt, RunAt: runAt, Status: Pending, CreatedAt: s.now()}
	s.mu.Lock()
	s.items[it.ID] = it
	s.mu.Unlock()
	s.save()
	return *it, nil
}

// List returns items for a session ("" for all), ordered by run time.
func (s *Scheduler) List(sessionID string) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Item{}
	for _, it := range s.items {
		if sessionID == "" || it.SessionID == sessionID {
			out = append(out, *it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunAt.Before(out[j].RunAt) })
	return out
}

// Delete removes an item.
func (s *Scheduler) Delete(id string) bool {
	s.mu.Lock()
	_, ok := s.items[id]
	delete(s.items, id)
	s.mu.Unlock()
	if ok {
		s.save()
	}
	return ok
}

// DeleteSession removes all items of a session.
func (s *Scheduler) DeleteSession(sessionID string) {
	s.mu.Lock()
	for id, it := range s.items {
		if it.SessionID == sessionID {
			delete(s.items, id)
		}
	}
	s.mu.Unlock()
	s.save()
}

// Run ticks until stop is closed.
func (s *Scheduler) Run(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.Tick()
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// Tick fires every due pending item. Items are marked running before firing
// so a slow fire cannot be picked up twice by overlapping ticks.
func (s *Scheduler) Tick() {
	now := s.now()
	var due []*Item
	s.mu.Lock()
	for _, it := range s.items {
		if it.Status == Pending && !it.RunAt.After(now) {
			it.Status = "running"
			due = append(due, it)
		}
	}
	s.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].RunAt.Before(due[j].RunAt) })
	for _, it := range due {
		err := s.fire(it)
		s.mu.Lock()
		if err != nil {
			it.Status, it.Error = Failed, err.Error()
			log.Printf("[schedule %s] failed: %v", it.ID, err)
		} else {
			it.Status = Done
		}
		snapshot := *it
		s.mu.Unlock()
		s.save()
		if s.OnFire != nil {
			s.OnFire(snapshot)
		}
	}
}

func (s *Scheduler) fire(it *Item) error {
	started, err := s.target.Ensure(it.SessionID)
	if err != nil {
		return err
	}
	if started {
		// 再開直後のClaude CodeはTUIの初期化中で入力を取りこぼすため待つ
		time.Sleep(s.StartupDelay)
	}
	if err := s.target.Write(it.SessionID, EncodePrompt(it.Prompt)); err != nil {
		return err
	}
	time.Sleep(s.EnterDelay)
	return s.target.Write(it.SessionID, []byte("\r"))
}

// EncodePrompt prepares prompt text for the TUI input box. The submitting
// Enter is sent separately by the caller.
func EncodePrompt(p string) []byte {
	// 改行を含むテキストをそのまま送ると途中の改行で送信されてしまうため、bracketed pasteとして送る
	if strings.ContainsAny(p, "\r\n") {
		return []byte("\x1b[200~" + p + "\x1b[201~")
	}
	return []byte(p)
}

func (s *Scheduler) save() {
	s.mu.Lock()
	items := make([]*Item, 0, len(s.items))
	for _, it := range s.items {
		cp := *it
		// 実行中にプロセスが落ちた場合に再実行されないよう、runningはfailedとして保存する
		if cp.Status == "running" {
			cp.Status, cp.Error = Failed, "interrupted"
		}
		items = append(items, &cp)
	}
	s.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].RunAt.Before(items[j].RunAt) })
	b, _ := json.MarshalIndent(items, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		os.Rename(tmp, s.path)
	}
}
