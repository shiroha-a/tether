package fsapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	// MaxUpload is the maximum total request body size for uploads.
	MaxUpload = 100 << 20
	// MaxPreview is the maximum number of bytes returned by the preview endpoint.
	MaxPreview = 1 << 20
)

// Handler serves the /api/fs/* endpoints.
type Handler struct {
	Root string
	// Home expands "~/" in stat requests; empty means the current user's home.
	Home string
}

// Entry is one item of a directory listing.
type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"isDir"`
	IsLink  bool      `json:"isLink"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

// Register mounts the handlers on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/fs/list", h.list)
	mux.HandleFunc("POST /api/fs/mkdir", h.mkdir)
	mux.HandleFunc("GET /api/fs/download", h.download)
	mux.HandleFunc("POST /api/fs/upload", h.upload)
	mux.HandleFunc("GET /api/fs/preview", h.preview)
	mux.HandleFunc("POST /api/fs/stat", h.stat)
}

// maxStatPaths bounds one stat request (a chat message rarely mentions more).
const maxStatPaths = 100

// StatResult describes one existing path found by the stat endpoint.
type StatResult struct {
	Input string `json:"input"`
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
}

// stat serves POST /api/fs/stat: it resolves path candidates mentioned in
// chat or terminal output and returns the ones that exist inside the root.
// "~/" is the home directory and relative paths are resolved against base.
func (h *Handler) stat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Base  string   `json:"base"`
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if len(body.Paths) > maxStatPaths {
		http.Error(w, "too many paths", http.StatusBadRequest)
		return
	}
	// 基準フォルダもルート内に限る（ルート外を基準に相対パスを解決させない）
	base, err := Resolve(h.Root, body.Base)
	if err != nil {
		writeFSError(w, err)
		return
	}
	home := h.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	results := []StatResult{}
	for _, in := range body.Paths {
		p := in
		switch {
		case p == "" || strings.ContainsRune(p, 0):
			continue
		case p == "~" || strings.HasPrefix(p, "~/"):
			if home == "" {
				continue
			}
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		case !filepath.IsAbs(p):
			p = filepath.Join(base, p)
		}
		// Resolveはシンボリックリンクを解決したうえでルート内かを確かめる。ルート外や存在しないものは返さない
		real, err := Resolve(h.Root, p)
		if err != nil {
			continue
		}
		info, err := os.Stat(real)
		if err != nil {
			continue
		}
		results = append(results, StatResult{Input: in, Path: real, IsDir: info.IsDir(), Size: info.Size()})
	}
	writeJSON(w, http.StatusOK, results)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	dir, err := Resolve(h.Root, r.URL.Query().Get("path"))
	if err != nil {
		writeFSError(w, err)
		return
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		writeFSError(w, err)
		return
	}
	hidden := r.URL.Query().Get("hidden") == "1"
	entries := make([]Entry, 0, len(des))
	for _, de := range des {
		if !hidden && strings.HasPrefix(de.Name(), ".") {
			continue
		}
		full := filepath.Join(dir, de.Name())
		e := Entry{Name: de.Name(), Path: full, IsLink: de.Type()&fs.ModeSymlink != 0}
		// リンク先の種別で表示したいのでStat（リンクを辿る）を使う。壊れたリンクはLstatの情報で出す
		info, err := os.Stat(full)
		if err != nil {
			info, err = de.Info()
			if err != nil {
				continue
			}
		}
		e.IsDir = info.IsDir()
		e.Size = info.Size()
		e.ModTime = info.ModTime()
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	parent := ""
	if dir != h.Root {
		parent = filepath.Dir(dir)
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": dir, "parent": parent, "root": h.Root, "entries": entries})
}

func (h *Handler) mkdir(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	target, err := ResolveNew(h.Root, body.Path)
	if err != nil {
		writeFSError(w, err)
		return
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		writeFSError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": target})
}

// inlineTypes are served inline for previews. SVG and HTML are excluded
// because they can run script in the app's origin.
var inlineTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".pdf": "application/pdf",
}

func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	p, err := Resolve(h.Root, r.URL.Query().Get("path"))
	if err != nil {
		writeFSError(w, err)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeFSError(w, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "not a regular file", http.StatusBadRequest)
		return
	}
	name := filepath.Base(p)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; style-src 'unsafe-inline'")
	ct, inline := inlineTypes[strings.ToLower(filepath.Ext(name))]
	if inline && r.URL.Query().Get("inline") == "1" {
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	}
	http.ServeContent(w, r, "", info.ModTime(), f)
}

func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	dir, err := Resolve(h.Root, r.URL.Query().Get("dir"))
	if err != nil {
		writeFSError(w, err)
		return
	}
	overwrite := r.URL.Query().Get("overwrite") == "1"
	r.Body = http.MaxBytesReader(w, r.Body, MaxUpload)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "expected multipart body", http.StatusBadRequest)
		return
	}
	var saved []string
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeFSError(w, err)
			return
		}
		if part.FileName() == "" {
			continue
		}
		name := filepath.Base(part.FileName())
		if err := ValidName(name); err != nil {
			writeFSError(w, err)
			return
		}
		target := filepath.Join(dir, name)
		// O_NOFOLLOWで、既存のシンボリックリンク経由でROOT外に書き込まれるのを防ぐ
		flags := os.O_WRONLY | os.O_CREATE | syscall.O_NOFOLLOW
		if overwrite {
			flags |= os.O_TRUNC
		} else {
			flags |= os.O_EXCL
		}
		f, err := os.OpenFile(target, flags, 0o644)
		if err != nil {
			writeFSError(w, err)
			return
		}
		_, copyErr := io.Copy(f, part)
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil {
			// 途中で切れたファイルを残さない
			os.Remove(target)
			writeFSError(w, errors.Join(copyErr, closeErr))
			return
		}
		saved = append(saved, target)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"saved": saved})
}

func (h *Handler) preview(w http.ResponseWriter, r *http.Request) {
	p, err := Resolve(h.Root, r.URL.Query().Get("path"))
	if err != nil {
		writeFSError(w, err)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeFSError(w, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "not a regular file", http.StatusBadRequest)
		return
	}
	buf, err := io.ReadAll(io.LimitReader(f, MaxPreview))
	if err != nil {
		writeFSError(w, err)
		return
	}
	truncated := info.Size() > MaxPreview
	if truncated {
		// 切り詰めでUTF-8の途中を切った場合に備え、末尾の不完全なルーンを落とす
		for i := 0; i < utf8.UTFMax-1 && len(buf) > 0 && !utf8.Valid(buf); i++ {
			buf = buf[:len(buf)-1]
		}
	}
	if bytes.IndexByte(buf, 0) >= 0 || !utf8.Valid(buf) {
		http.Error(w, "binary file cannot be previewed", http.StatusUnsupportedMediaType)
		return
	}
	kind := "text"
	switch strings.ToLower(filepath.Ext(p)) {
	case ".md", ".markdown":
		kind = "markdown"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": p, "name": filepath.Base(p), "size": info.Size(), "modTime": info.ModTime(),
		"kind": kind, "content": string(buf), "truncated": truncated,
	})
}

func writeFSError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	switch {
	case errors.Is(err, ErrOutsideRoot):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, ErrBadName):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.As(err, &maxErr):
		http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, fs.ErrExist):
		http.Error(w, "already exists", http.StatusConflict)
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.ELOOP):
		http.Error(w, "permission denied", http.StatusForbidden)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
