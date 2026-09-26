package peer

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditEntry records one call made by a client.
type AuditEntry struct {
	At         time.Time `json:"at"`
	Client     string    `json:"client"`
	ClientID   string    `json:"clientId"`
	Op         string    `json:"op"`
	Detail     string    `json:"detail,omitempty"`
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
	DurationMs int64     `json:"durationMs"`
}

// Audit appends entries to <dataDir>/peer-audit.jsonl and keeps it bounded.
type Audit struct {
	mu    sync.Mutex
	path  string
	max   int
	lines int
}

const maxAuditDetail = 500

// NewAudit opens the audit log in dataDir, keeping at most max entries.
func NewAudit(dataDir string, max int) (*Audit, error) {
	a := &Audit{path: filepath.Join(dataDir, "peer-audit.jsonl"), max: max}
	entries, err := a.read()
	if err != nil {
		return nil, err
	}
	a.lines = len(entries)
	return a, nil
}

// Add appends an entry. Long details are shortened.
func (a *Audit) Add(e AuditEntry) error {
	if r := []rune(e.Detail); len(r) > maxAuditDetail {
		e.Detail = string(r[:maxAuditDetail]) + "…"
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	a.lines++
	if a.lines > a.max {
		return a.trimLocked()
	}
	return nil
}

// Recent returns up to n entries, newest first.
func (a *Audit) Recent(n int) ([]AuditEntry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entries, err := a.read()
	if err != nil {
		return nil, err
	}
	out := make([]AuditEntry, 0, min(n, len(entries)))
	for i := len(entries) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, entries[i])
	}
	return out, nil
}

// trimLocked keeps the newest half of the limit, so trimming is rare.
func (a *Audit) trimLocked() error {
	entries, err := a.read()
	if err != nil {
		return err
	}
	keep := entries[max(0, len(entries)-a.max/2):]
	tmp := a.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, e := range keep {
		enc.Encode(e)
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, a.path); err != nil {
		return err
	}
	a.lines = len(keep)
	return nil
}

func (a *Audit) read() ([]AuditEntry, error) {
	f, err := os.Open(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e AuditEntry
		// 壊れた行（書き込み中の停止など）は飛ばす
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
