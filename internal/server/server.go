// Package server wires the HTTP API, WebSocket endpoints and static UI.
package server

import (
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	"tether/internal/auth"
	"tether/internal/events"
	"tether/internal/fsapi"
	"tether/internal/guard"
	"tether/internal/schedule"
	"tether/internal/session"
	"tether/internal/sysinfo"
	"tether/internal/transcript"
	"tether/internal/usage"
)

// Server holds the dependencies of the HTTP handlers.
type Server struct {
	root      string
	token     string
	sessions  *session.Manager
	hub       *events.Hub
	notifier  *events.Notifier
	scheduler *schedule.Scheduler
	usage     *usage.Client
	static    fs.FS
	hosts     *guard.Hosts
	// transcripts locates Claude Code transcript files by conversation id.
	transcripts  func(claudeSessionID string) (string, bool)
	startupDelay time.Duration
	enterDelay   time.Duration
	version      string
	repository   string
	startedAt    time.Time
}

// Deps are the components the server needs.
type Deps struct {
	Root      string
	Token     string
	Sessions  *session.Manager
	Hub       *events.Hub
	Notifier  *events.Notifier
	Scheduler *schedule.Scheduler
	Usage     *usage.Client
	// Static is the built web UI; nil disables it.
	Static fs.FS
	// Hosts is the Host header allowlist; nil allows only IP literals and localhost.
	Hosts *guard.Hosts
	// Transcripts locates Claude Code transcript files; nil disables the chat view.
	Transcripts func(claudeSessionID string) (string, bool)
	// StartupDelay is how long to wait after resuming a stopped session before
	// sending input (Claude Code drops keystrokes while its TUI starts).
	StartupDelay time.Duration
	// Version is the build version shown in the about dialog; empty means "dev".
	Version string
	// Repository is the source repository URL shown in the about dialog.
	Repository string
}

// New builds a server.
func New(d Deps) *Server {
	return &Server{
		root: d.Root, token: d.Token, sessions: d.Sessions, hub: d.Hub, notifier: d.Notifier,
		scheduler: d.Scheduler, usage: d.Usage, static: d.Static, hosts: d.Hosts,
		transcripts: d.Transcripts, startupDelay: d.StartupDelay, enterDelay: 300 * time.Millisecond,
		version: cmp.Or(d.Version, "dev"), repository: d.Repository, startedAt: time.Now(),
	}
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	(&fsapi.Handler{Root: s.root}).Register(api)
	api.HandleFunc("GET /api/config", s.config)
	api.HandleFunc("GET /api/about", s.about)
	api.HandleFunc("GET /api/sessions", s.listSessions)
	api.HandleFunc("POST /api/sessions", s.createSession)
	api.HandleFunc("PATCH /api/sessions/{id}", s.updateSession)
	api.HandleFunc("POST /api/sessions/{id}/stop", s.stopSession)
	api.HandleFunc("DELETE /api/sessions/{id}", s.deleteSession)
	api.HandleFunc("GET /api/sessions/{id}/transcript", s.transcript)
	api.HandleFunc("POST /api/sessions/{id}/input", s.sessionInput)
	api.HandleFunc("GET /api/schedules", s.listSchedules)
	api.HandleFunc("POST /api/schedules", s.createSchedule)
	api.HandleFunc("DELETE /api/schedules/{id}", s.deleteSchedule)
	api.HandleFunc("GET /api/usage", s.usage.Handler)
	api.HandleFunc("GET /api/notifications", s.notifier.HandleRecent)
	api.HandleFunc("GET /api/system", (&sysinfo.Reader{ProcDir: "/proc", DiskPath: s.root}).Handler)
	api.HandleFunc("GET /ws/sessions/{id}", s.terminal)
	api.HandleFunc("GET /ws/events", s.events)
	api.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })

	root := http.NewServeMux()
	root.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "authRequired": s.token != ""})
	})
	// hookはセッションごとのキーで認証するので、TETHER_TOKENの認証は通さない
	root.HandleFunc("POST /internal/hook", s.notifier.HandleHook)
	protected := auth.Middleware(s.token, api)
	root.Handle("/api/", protected)
	root.Handle("/ws/", protected)
	root.Handle("/", s.staticHandler())
	hosts := s.hosts
	if hosts == nil {
		hosts = guard.NewHosts()
	}
	// Host検証、CSP、Origin検証のすべてが元のホスト名を見るよう、最初に復元する
	return guard.UnixForwardedHost(securityHeaders(guard.Middleware(hosts, root)))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", contentSecurityPolicy(r.Host))
		next.ServeHTTP(w, r)
	})
}

// contentSecurityPolicy restricts the UI to its own origin. In particular
// img-src blocks remote images in rendered Markdown, which could otherwise
// leak data placed in the URL (for example by a prompt-injected reply).
func contentSecurityPolicy(host string) string {
	// Hostヘッダはguardで検証済みだが、念のためCSPの区切りになり得る文字を含むものは使わない
	ws := ""
	if host != "" && !strings.ContainsAny(host, " ;,'\"\r\n") {
		// 古いSafariは'self'をWebSocketに適用しないため、同じホストのws/wssを明示する
		ws = " ws://" + host + " wss://" + host
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		// xterm.jsとReactが要素のstyle属性を使うため
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob:",
		"font-src 'self' data:",
		"connect-src 'self'" + ws,
		"worker-src 'self'",
		"manifest-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
	}, "; ")
}

// staticHandler serves the SPA with index.html fallback for client routes.
func (s *Server) staticHandler() http.Handler {
	if s.static == nil {
		return http.NotFoundHandler()
	}
	files := http.FileServerFS(s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(s.static, p); err != nil {
			// クライアント側ルーティング用にindex.htmlへフォールバックする
			p = "index.html"
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		switch {
		case strings.HasPrefix(p, "assets/"):
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			// index.htmlとsw.jsは常に最新を取りに行かせ、更新が反映されない事故を防ぐ
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"root":            s.root,
		"permissionModes": session.PermissionModes,
		"models":          []string{"", "opus", "sonnet", "haiku"},
	})
}

func (s *Server) about(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    s.version,
		"goVersion":  runtime.Version(),
		"platform":   runtime.GOOS + "/" + runtime.GOARCH,
		"startedAt":  s.startedAt,
		"repository": s.repository,
		"license":    "MIT",
	})
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.sessions.List())
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind           string `json:"kind"`
		Cwd            string `json:"cwd"`
		Model          string `json:"model"`
		PermissionMode string `json:"permissionMode"`
		Label          string `json:"label"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	cwd, err := fsapi.Resolve(s.root, body.Cwd)
	if err != nil {
		http.Error(w, "cwd: "+err.Error(), http.StatusForbidden)
		return
	}
	if strings.ContainsAny(body.Model, " \t\n") {
		http.Error(w, "invalid model", http.StatusBadRequest)
		return
	}
	sess, err := s.sessions.Create(session.Options{Kind: body.Kind, Cwd: cwd, Model: body.Model, PermissionMode: body.PermissionMode, Label: strings.TrimSpace(body.Label)})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, sess.Status())
}

func (s *Server) updateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label *string `json:"label"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	err := s.sessions.Update(r.PathValue("id"), func(sp *session.Spec) {
		if body.Label != nil {
			sp.Label = strings.TrimSpace(*body.Label)
		}
	})
	if !sessionErr(w, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) stopSession(w http.ResponseWriter, r *http.Request) {
	if !sessionErr(w, s.sessions.Stop(r.PathValue("id"))) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if sessionErr(w, s.sessions.Delete(id)) {
		return
	}
	s.scheduler.DeleteSession(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.scheduler.List(r.URL.Query().Get("session")))
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string    `json:"sessionId"`
		Prompt    string    `json:"prompt"`
		RunAt     time.Time `json:"runAt"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	sess, err := s.sessions.Get(body.SessionID)
	if sessionErr(w, err) {
		return
	}
	if sess.Spec().IsShell() {
		// シェルへの定時入力は意図しないコマンド実行になりやすいので、予約はClaude Codeのセッションに限る
		http.Error(w, "scheduled prompts are only supported for Claude Code sessions", http.StatusBadRequest)
		return
	}
	it, err := s.scheduler.Add(body.SessionID, body.Prompt, body.RunAt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.hub.Publish(map[string]any{"type": "schedule.changed", "session": it.SessionID})
	writeJSON(w, http.StatusCreated, it)
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.scheduler.Delete(r.PathValue("id")) {
		http.NotFound(w, r)
		return
	}
	s.hub.Publish(map[string]any{"type": "schedule.changed"})
	w.WriteHeader(http.StatusNoContent)
}

// transcriptTail is how many items the first transcript request returns.
const transcriptTail = 300

func (s *Server) transcript(w http.ResponseWriter, r *http.Request) {
	sess, err := s.sessions.Get(r.PathValue("id"))
	if sessionErr(w, err) {
		return
	}
	spec := sess.Spec()
	if spec.IsShell() {
		http.Error(w, "chat view is only available for Claude Code sessions", http.StatusBadRequest)
		return
	}
	resp := map[string]any{"items": []transcript.Item{}, "offset": 0, "claudeSessionId": spec.ClaudeSessionID}
	path, ok := "", false
	if s.transcripts != nil {
		path, ok = s.transcripts(spec.ClaudeSessionID)
	}
	if !ok {
		// まだ一度も発言していない会話は記録ファイルがない
		writeJSON(w, http.StatusOK, resp)
		return
	}
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if offset < 0 {
		offset = 0
	}
	items, next, err := transcript.ReadFrom(path, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if items == nil {
		items = []transcript.Item{}
	}
	// 初回は長い会話でも重くならないよう末尾だけ返す
	if offset == 0 && len(items) > transcriptTail {
		items = items[len(items)-transcriptTail:]
		resp["truncated"] = true
	}
	resp["items"], resp["offset"] = items, next
	writeJSON(w, http.StatusOK, resp)
}

const (
	maxInputKeys = 20
	keyInterval  = 40 * time.Millisecond
)

// inputKeys are the only raw keys the chat view may send. Arbitrary byte
// sequences are not accepted so the endpoint cannot inject terminal escapes.
var inputKeys = map[string]string{
	"up": "\x1b[A", "down": "\x1b[B", "enter": "\r", "esc": "\x1b", "tab": "\t", "shift-tab": "\x1b[Z",
	"1": "1", "2": "2", "3": "3", "4": "4", "5": "5", "6": "6", "7": "7", "8": "8", "9": "9",
}

func (s *Server) sessionInput(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string   `json:"text"`
		Key  string   `json:"key"`
		Keys []string `json:"keys"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	text := strings.TrimRight(body.Text, "\r\n")
	keys := body.Keys
	if body.Key != "" {
		keys = append([]string{body.Key}, keys...)
	}
	// 選択肢の移動（↑↓を何回か押してEnter）に使う。長い連打は受け付けない
	if len(keys) > maxInputKeys {
		http.Error(w, "too many keys", http.StatusBadRequest)
		return
	}
	var seqs []string
	for _, k := range keys {
		seq, ok := inputKeys[k]
		if !ok {
			http.Error(w, "unknown key", http.StatusBadRequest)
			return
		}
		seqs = append(seqs, seq)
	}
	isKey := len(seqs) > 0
	if !isKey && strings.TrimSpace(text) == "" {
		http.Error(w, "text or key is required", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	sess, err := s.sessions.Get(id)
	if sessionErr(w, err) {
		return
	}
	if !sess.Running() {
		if _, err := s.sessions.Ensure(id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// 再開直後のTUIは入力を取りこぼすので、起動を待ってから送る
		time.Sleep(s.startupDelay)
	}
	if isKey {
		for i, seq := range seqs {
			if i > 0 {
				// TUIがキーを1つずつ処理できるよう、少し間を空ける
				time.Sleep(keyInterval)
			}
			if err = sess.Write([]byte(seq)); err != nil {
				break
			}
		}
	} else if err = sess.Write(schedule.EncodePrompt(text)); err == nil {
		time.Sleep(s.enterDelay)
		err = sess.Write([]byte("\r"))
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sessionErr writes an error response and reports whether err was non-nil.
func sessionErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, session.ErrNotFound):
		http.Error(w, "session not found", http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	return true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
