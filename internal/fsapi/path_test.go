package fsapi

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// setupRoot creates root/{sub/file.txt} plus an outside dir and symlinks into it.
func setupRoot(t *testing.T) (root, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(base, "root")
	outside = filepath.Join(base, "rootevil")
	for _, d := range []string{filepath.Join(root, "sub"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "file.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "sub"), filepath.Join(root, "inner")); err != nil {
		t.Fatal(err)
	}
	return root, outside
}

func TestResolve(t *testing.T) {
	root, outside := setupRoot(t)
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr error
	}{
		{"empty is root", "", root, nil},
		{"root itself", root, root, nil},
		{"child", filepath.Join(root, "sub"), filepath.Join(root, "sub"), nil},
		{"inner symlink", filepath.Join(root, "inner", "file.txt"), filepath.Join(root, "sub", "file.txt"), nil},
		{"dotdot escape", filepath.Join(root, "sub", "..", ".."), "", ErrOutsideRoot},
		{"sibling with common prefix", outside, "", ErrOutsideRoot},
		{"symlink escape", filepath.Join(root, "escape"), "", ErrOutsideRoot},
		{"relative path", "sub", "", ErrOutsideRoot},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Resolve(root, c.in)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got (%q, %v), want %q", got, err, c.want)
			}
		})
	}
}

func TestResolveNew(t *testing.T) {
	root, _ := setupRoot(t)
	got, err := ResolveNew(root, filepath.Join(root, "inner", "new.txt"))
	if err != nil || got != filepath.Join(root, "sub", "new.txt") {
		t.Fatalf("got (%q, %v)", got, err)
	}
	if _, err := ResolveNew(root, filepath.Join(root, "escape", "x")); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("symlinked parent: err = %v", err)
	}
	if _, err := ResolveNew(root, root+"/sub/.."); err == nil {
		t.Fatalf("clean to root parent should not create root sibling")
	}
}

func TestValidName(t *testing.T) {
	for _, n := range []string{"", ".", "..", "a/b", "a\x00b"} {
		if ValidName(n) == nil {
			t.Errorf("ValidName(%q) = nil, want error", n)
		}
	}
	if err := ValidName("ok.txt"); err != nil {
		t.Errorf("ValidName(ok.txt) = %v", err)
	}
}
