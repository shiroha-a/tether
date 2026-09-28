package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"tether/internal/transcript"
)

const (
	// maxImageUpload is the size limit of one image attached from the chat view.
	maxImageUpload = 10 << 20
	// maxInputImages is how many images one prompt may carry.
	maxInputImages = 10
	// uploadTTL is how long attached images are kept (Claude Code reads them when the prompt is sent).
	uploadTTL = 7 * 24 * time.Hour
)

// uploadExt maps the detected content type of an attached image to its file extension.
var uploadExt = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}

// uploadName is the form of the names given to attached images; input only accepts these.
var uploadName = regexp.MustCompile(`^[0-9a-f]{32}\.(png|jpg|gif|webp)$`)

// sessionTranscript resolves the transcript file of a Claude Code session and
// writes an error response when there is none.
func (s *Server) sessionTranscript(w http.ResponseWriter, r *http.Request) (string, bool) {
	sess, err := s.sessions.Get(r.PathValue("id"))
	if sessionErr(w, err) {
		return "", false
	}
	spec := sess.Spec()
	if spec.IsShell() || s.transcripts == nil {
		http.Error(w, "chat view is only available for Claude Code sessions", http.StatusBadRequest)
		return "", false
	}
	path, ok := s.transcripts(spec.ClaudeSessionID)
	if !ok {
		http.NotFound(w, r)
		return "", false
	}
	return path, true
}

// transcriptImage serves an image block of the session's transcript.
func (s *Server) transcriptImage(w http.ResponseWriter, r *http.Request) {
	path, ok := s.sessionTranscript(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	line, err1 := strconv.ParseInt(q.Get("line"), 10, 64)
	index, err2 := strconv.Atoi(q.Get("index"))
	sub, err3 := strconv.Atoi(q.Get("sub"))
	if err := errors.Join(err1, err2, err3); err != nil {
		http.Error(w, "line, index and sub are required", http.StatusBadRequest)
		return
	}
	typ, data, err := transcript.ReadImage(path, transcript.ImageRef{Line: line, Index: index, Sub: sub})
	if errors.Is(err, transcript.ErrNoImage) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "failed to read the transcript", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", typ)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// 記録は追記されるだけなので、同じ位置の画像は変わらない
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(data)
}

// sessionAgents reports the progress of the subagents started by the given tool calls (?toolUseIds=a,b).
func (s *Server) sessionAgents(w http.ResponseWriter, r *http.Request) {
	path, ok := s.sessionTranscript(w, r)
	if !ok {
		return
	}
	ids := strings.Split(r.URL.Query().Get("toolUseIds"), ",")
	if len(ids) > 50 {
		ids = ids[:50]
	}
	writeJSON(w, http.StatusOK, s.agents.Read(path, ids))
}

// uploadImage stores an image attached in the chat view until it is sent.
func (s *Server) uploadImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.sessions.Get(id); sessionErr(w, err) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImageUpload+64<<10)
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "an image file up to 10MB is required", http.StatusBadRequest)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxImageUpload+1))
	if err != nil || len(data) > maxImageUpload {
		http.Error(w, "an image file up to 10MB is required", http.StatusBadRequest)
		return
	}
	// 拡張子や申告された形式ではなく、中身で判定する
	ext, ok := uploadExt[http.DetectContentType(data)]
	if !ok {
		http.Error(w, "only PNG, JPEG, GIF and WebP images are supported", http.StatusBadRequest)
		return
	}
	s.cleanUploads()
	dir := filepath.Join(s.uploadDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("uploads: %v", err)
		http.Error(w, "failed to save the image", http.StatusInternalServerError)
		return
	}
	b := make([]byte, 16)
	rand.Read(b)
	name := hex.EncodeToString(b) + ext
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		log.Printf("uploads: %v", err)
		http.Error(w, "failed to save the image", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": name})
}

// uploadedImages resolves image ids sent with a prompt to their files.
func (s *Server) uploadedImages(sessionID string, ids []string) ([]string, error) {
	if len(ids) > maxInputImages {
		return nil, errors.New("too many images")
	}
	var paths []string
	for _, id := range ids {
		if s.uploadDir == "" || !uploadName.MatchString(id) {
			return nil, errors.New("unknown image")
		}
		p := filepath.Join(s.uploadDir, sessionID, id)
		if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("unknown image")
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// cleanUploads removes attached images older than uploadTTL.
func (s *Server) cleanUploads() {
	cutoff := time.Now().Add(-uploadTTL)
	filepath.WalkDir(s.uploadDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(p)
		}
		return nil
	})
}
