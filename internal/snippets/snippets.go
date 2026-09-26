// Package snippets stores frequently used shell commands shared by all devices.
package snippets

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Limits on stored snippets.
const (
	MaxItems        = 100
	MaxCommandBytes = 1000
	MaxLabelRunes   = 60
)

// Errors returned by Store. Other errors come from saving the file.
var (
	ErrNotFound = errors.New("snippet not found")
	ErrInvalid  = errors.New("invalid snippet")
)

// Item is one registered command.
type Item struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Command   string    `json:"command"`
	CreatedAt time.Time `json:"createdAt"`
}

// Store keeps snippets in registration order and persists them as JSON.
type Store struct {
	mu    sync.Mutex
	items []Item
	path  string
	now   func() time.Time
}

// New loads snippets from dataDir.
func New(dataDir string) (*Store, error) {
	s := &Store{path: filepath.Join(dataDir, "snippets.json"), now: time.Now}
	b, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.items); err != nil {
			return nil, fmt.Errorf("parse %s: %w", s.path, err)
		}
	}
	return s, nil
}

// List returns all snippets in registration order.
func (s *Store) List() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Item{}, s.items...)
}

// Add validates and stores a new snippet.
func (s *Store) Add(label, command string) (Item, error) {
	label, command, err := normalize(label, command)
	if err != nil {
		return Item{}, err
	}
	b := make([]byte, 8)
	rand.Read(b)
	it := Item{ID: hex.EncodeToString(b), Label: label, Command: command, CreatedAt: s.now()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.items) >= MaxItems {
		return Item{}, fmt.Errorf("%w: too many snippets (max %d)", ErrInvalid, MaxItems)
	}
	if err := s.commitLocked(append(slices.Clone(s.items), it)); err != nil {
		return Item{}, err
	}
	return it, nil
}

// Update replaces the label and command of a snippet.
func (s *Store) Update(id, label, command string) (Item, error) {
	label, command, err := normalize(label, command)
	if err != nil {
		return Item{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexLocked(id)
	if i < 0 {
		return Item{}, ErrNotFound
	}
	next := slices.Clone(s.items)
	next[i].Label, next[i].Command = label, command
	if err := s.commitLocked(next); err != nil {
		return Item{}, err
	}
	return next[i], nil
}

// Delete removes a snippet.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexLocked(id)
	if i < 0 {
		return ErrNotFound
	}
	return s.commitLocked(slices.Delete(slices.Clone(s.items), i, i+1))
}

// normalize trims and validates a snippet. Commands are sent to a shell
// followed by Enter, so they must be a single line without control characters.
func normalize(label, command string) (string, string, error) {
	label, command = strings.TrimSpace(label), strings.TrimSpace(command)
	switch {
	case command == "":
		return "", "", fmt.Errorf("%w: command is empty", ErrInvalid)
	case len(command) > MaxCommandBytes:
		return "", "", fmt.Errorf("%w: command is too long (max %d bytes)", ErrInvalid, MaxCommandBytes)
	case !utf8.ValidString(command) || hasControl(command):
		// 改行や制御文字が入っていると、1回のタップで複数のコマンドやキー操作が送られてしまう
		return "", "", fmt.Errorf("%w: command must be a single line without control characters", ErrInvalid)
	case utf8.RuneCountInString(label) > MaxLabelRunes:
		return "", "", fmt.Errorf("%w: label is too long (max %d characters)", ErrInvalid, MaxLabelRunes)
	case !utf8.ValidString(label) || hasControl(label):
		return "", "", fmt.Errorf("%w: label must not contain control characters", ErrInvalid)
	}
	return label, command, nil
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

func (s *Store) indexLocked(id string) int {
	return slices.IndexFunc(s.items, func(it Item) bool { return it.ID == id })
}

// commitLocked writes items to disk and only then makes them current, so a
// failed write leaves memory and disk in agreement.
func (s *Store) commitLocked(items []Item) error {
	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.items = items
	return nil
}
