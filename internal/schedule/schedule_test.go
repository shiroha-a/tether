package schedule

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeTarget struct {
	mu       sync.Mutex
	running  map[string]bool
	writes   []string
	ensureOK bool
}

func (f *fakeTarget) Ensure(id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ensureOK {
		return false, errors.New("no such session")
	}
	started := !f.running[id]
	f.running[id] = true
	return started, nil
}

func (f *fakeTarget) Write(id string, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, id+":"+string(p))
	return nil
}

func newTest(t *testing.T, target *fakeTarget) (*Scheduler, *time.Time) {
	t.Helper()
	s, err := New(t.TempDir(), target)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.StartupDelay, s.EnterDelay = 0, 0
	return s, &now
}

func TestTickFiresOnlyDueItems(t *testing.T) {
	ft := &fakeTarget{running: map[string]bool{"s1": true}, ensureOK: true}
	s, now := newTest(t, ft)
	due, _ := s.Add("s1", "run tests\n", *now)
	later, _ := s.Add("s1", "later", now.Add(time.Second))

	s.Tick()
	if len(ft.writes) != 2 || ft.writes[0] != "s1:run tests" || ft.writes[1] != "s1:\r" {
		t.Fatalf("writes = %q", ft.writes)
	}
	st := map[string]string{}
	for _, it := range s.List("s1") {
		st[it.ID] = it.Status
	}
	if st[due.ID] != Done || st[later.ID] != Pending {
		t.Fatalf("statuses = %v", st)
	}
	// 実行済みは二度と実行しない
	s.Tick()
	if len(ft.writes) != 2 {
		t.Fatalf("refired: %q", ft.writes)
	}
	*now = now.Add(time.Second)
	s.Tick()
	if len(ft.writes) != 4 {
		t.Fatalf("later item not fired at its time: %q", ft.writes)
	}
}

func TestMultilinePromptUsesBracketedPaste(t *testing.T) {
	ft := &fakeTarget{running: map[string]bool{}, ensureOK: true}
	s, now := newTest(t, ft)
	s.Add("s1", "line1\nline2", *now)
	s.Tick()
	if len(ft.writes) != 2 || ft.writes[0] != "s1:\x1b[200~line1\nline2\x1b[201~" {
		t.Fatalf("writes = %q", ft.writes)
	}
}

func TestFailedEnsureMarksFailed(t *testing.T) {
	ft := &fakeTarget{running: map[string]bool{}, ensureOK: false}
	s, now := newTest(t, ft)
	var fired []Item
	s.OnFire = func(it Item) { fired = append(fired, it) }
	s.Add("gone", "x", *now)
	s.Tick()
	if len(fired) != 1 || fired[0].Status != Failed || !strings.Contains(fired[0].Error, "no such session") {
		t.Fatalf("fired = %+v", fired)
	}
	if len(ft.writes) != 0 {
		t.Fatalf("wrote to missing session: %q", ft.writes)
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	ft := &fakeTarget{running: map[string]bool{}, ensureOK: true}
	s1, _ := New(dir, ft)
	it, err := s1.Add("s1", "hello", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	s2, err := New(dir, ft)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.List("")
	if len(got) != 1 || got[0].ID != it.ID || got[0].Status != Pending {
		t.Fatalf("reloaded = %+v", got)
	}
	s2.DeleteSession("s1")
	if s3, _ := New(dir, ft); len(s3.List("")) != 0 {
		t.Fatal("DeleteSession not persisted")
	}
}

func TestAddValidation(t *testing.T) {
	s, now := newTest(t, &fakeTarget{})
	if _, err := s.Add("s1", "  \n", *now); err == nil {
		t.Fatal("blank prompt accepted")
	}
	if _, err := s.Add("s1", "x", time.Time{}); err == nil {
		t.Fatal("zero runAt accepted")
	}
}
