package snippets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// same compares items; CreatedAt is compared with Equal because a reloaded
// time has no monotonic clock reading.
func same(a, b Item) bool {
	return a.ID == b.ID && a.Label == b.Label && a.Command == b.Command && a.CreatedAt.Equal(b.CreatedAt)
}

func TestAddListPersist(t *testing.T) {
	s, dir := newStore(t)
	if got := s.List(); got == nil || len(got) != 0 {
		t.Fatalf("empty store should list [] (not null): %#v", got)
	}
	a, err := s.Add("  状態  ", "  git status  ")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.Label != "状態" || a.Command != "git status" || a.CreatedAt.IsZero() {
		t.Fatalf("added = %+v", a)
	}
	b, _ := s.Add("", "ls -la")
	if a.ID == b.ID {
		t.Fatal("ids must be unique")
	}
	got := s.List()
	if len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID {
		t.Fatalf("list should keep registration order: %+v", got)
	}
	// Listの戻り値を書き換えても保存内容は変わらない
	got[0].Command = "rm -rf /"
	if s.List()[0].Command != "git status" {
		t.Fatal("List must return a copy")
	}

	info, err := os.Stat(filepath.Join(dir, "snippets.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("snippets.json: %v %v", info, err)
	}
	re, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded := re.List(); len(loaded) != 2 || !same(loaded[0], a) || !same(loaded[1], b) {
		t.Fatalf("reloaded = %+v", loaded)
	}
}

func TestUpdateAndDelete(t *testing.T) {
	s, dir := newStore(t)
	a, _ := s.Add("a", "echo a")
	b, _ := s.Add("b", "echo b")
	c, _ := s.Add("c", "echo c")

	u, err := s.Update(b.ID, " B ", " echo B ")
	if err != nil || u.ID != b.ID || u.Label != "B" || u.Command != "echo B" || !u.CreatedAt.Equal(b.CreatedAt) {
		t.Fatalf("update = %+v %v", u, err)
	}
	if _, err := s.Update("missing", "x", "echo x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	if _, err := s.Update(a.ID, "", "echo\nrm"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("update must validate: %v", err)
	}
	if err := s.Delete(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	got := s.List()
	if len(got) != 2 || got[0].ID != a.ID || got[1].ID != c.ID || got[0].Command != "echo a" {
		t.Fatalf("after delete = %+v", got)
	}
	re, _ := New(dir)
	if loaded := re.List(); len(loaded) != 2 || loaded[1].ID != c.ID {
		t.Fatalf("reloaded = %+v", loaded)
	}
}

func TestValidation(t *testing.T) {
	s, _ := newStore(t)
	bad := []struct{ name, label, command string }{
		{"empty", "x", ""},
		{"blank", "x", "   "},
		{"newline", "", "echo a\nrm -rf ~"},
		{"carriage return", "", "echo a\rrm"},
		{"tab", "", "echo\tx"},
		{"escape", "", "echo \x1b[31m"},
		{"ctrl-c", "", "sleep 1\x03"},
		{"del", "", "echo \x7f"},
		{"c1 control", "", "echo \u0085 x"},
		{"too long", "", strings.Repeat("a", MaxCommandBytes+1)},
		{"label too long", strings.Repeat("あ", MaxLabelRunes+1), "ls"},
		{"label control", "a\nb", "ls"},
	}
	for _, c := range bad {
		if _, err := s.Add(c.label, c.command); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", c.name, err)
		}
	}
	if n := len(s.List()); n != 0 {
		t.Fatalf("rejected items were stored: %d", n)
	}
	// 上限ちょうどは通る。日本語のラベルは文字数で数える
	if _, err := s.Add(strings.Repeat("あ", MaxLabelRunes), strings.Repeat("a", MaxCommandBytes)); err != nil {
		t.Fatalf("limits: %v", err)
	}
	if _, err := s.Add("", "echo 'a b' | grep \"x\" && cd ~/dev; ls $HOME"); err != nil {
		t.Fatalf("ordinary shell syntax: %v", err)
	}
}

func TestMaxItems(t *testing.T) {
	s, _ := newStore(t)
	for i := range MaxItems {
		if _, err := s.Add("", "echo "+strings.Repeat("x", i+1)); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if _, err := s.Add("", "echo over"); !errors.Is(err, ErrInvalid) {
		t.Fatal("more than MaxItems accepted")
	}
	if n := len(s.List()); n != MaxItems {
		t.Fatalf("len = %d", n)
	}
}

func TestFailedWriteKeepsState(t *testing.T) {
	s, dir := newStore(t)
	a, _ := s.Add("a", "echo a")
	// 保存先を書き込めなくする
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	_, err := s.Add("b", "echo b")
	if err == nil {
		t.Skip("directory is still writable (running as root?)")
	}
	if errors.Is(err, ErrInvalid) || errors.Is(err, ErrNotFound) {
		t.Fatalf("write failure must not look like a client error: %v", err)
	}
	if _, err := s.Update(a.ID, "a", "echo changed"); err == nil {
		t.Fatal("update should fail")
	}
	if err := s.Delete(a.ID); err == nil {
		t.Fatal("delete should fail")
	}
	if got := s.List(); len(got) != 1 || !same(got[0], a) {
		t.Fatalf("memory changed after failed writes: %+v", got)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "snippets.json"), []byte("{broken"), 0o600)
	if _, err := New(dir); err == nil {
		t.Fatal("broken file accepted")
	}
}
