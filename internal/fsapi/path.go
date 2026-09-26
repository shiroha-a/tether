// Package fsapi implements the file-system REST API confined to a root directory.
package fsapi

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrOutsideRoot is returned when a path resolves outside the configured root.
var ErrOutsideRoot = errors.New("path is outside the allowed root")

// ErrBadName is returned for file names that are empty or refer to "." or "..".
var ErrBadName = errors.New("invalid file name")

// Resolve returns the symlink-free absolute path of an existing path p,
// failing if it lies outside root. An empty p means root itself.
// root must already be symlink-free.
func Resolve(root, p string) (string, error) {
	if p == "" {
		return root, nil
	}
	if !filepath.IsAbs(p) {
		return "", ErrOutsideRoot
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil {
		return "", err
	}
	if !Within(root, real) {
		return "", ErrOutsideRoot
	}
	return real, nil
}

// ResolveNew validates a path that may not exist yet (mkdir, upload target).
// The parent must exist inside root and the final element must be a plain name.
func ResolveNew(root, p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", ErrOutsideRoot
	}
	p = filepath.Clean(p)
	name := filepath.Base(p)
	if err := ValidName(name); err != nil {
		return "", err
	}
	parent, err := Resolve(root, filepath.Dir(p))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, name), nil
}

// ValidName rejects names that could traverse directories.
func ValidName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return ErrBadName
	}
	return nil
}

// Within reports whether p equals root or is contained in it.
func Within(root, p string) bool {
	if p == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
		prefix += string(os.PathSeparator)
	}
	return strings.HasPrefix(p, prefix)
}
