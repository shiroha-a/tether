package peer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"tether/internal/fsapi"
)

// Limits on what a client can read or send.
const (
	MaxReadFile  = 512 << 10
	maxPrompt    = 20 << 10
	maxCommand   = 8 << 10
	maxBodyBytes = 64 << 10
)

// DelegateStatus is the state of a session created by Delegate.
type DelegateStatus struct {
	Session string `json:"session"`
	// State is "working", "waiting", "idle", "stopped" or "starting".
	State string `json:"state"`
	// Detail explains a waiting state (what is being asked).
	Detail  string    `json:"detail,omitempty"`
	Reply   string    `json:"reply,omitempty"`
	ReplyAt time.Time `json:"replyAt,omitzero"`
}

// Delegator starts Claude Code sessions on this machine for a client.
type Delegator interface {
	// Delegate creates a session in dir and sends prompt to it.
	Delegate(dir, prompt, permissionMode, label string) (string, error)
	// DelegateStatus reports a delegated session's state and last reply.
	DelegateStatus(session string) (DelegateStatus, error)
}

// Service serves /peer/v1/* for clients, enforcing their policies and
// recording every call.
type Service struct {
	Store     *Store
	Audit     *Audit
	Root      string
	Version   string
	Shell     string
	Env       []string
	Status    func() any
	Delegator Delegator
}

type clientKey struct{}

// Handler returns the handler for /peer/v1/*.
func (rm *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /peer/v1/hello", rm.hello)
	mux.HandleFunc("GET /peer/v1/status", rm.guard("status", func(p Policy) bool { return p.Status }, rm.status))
	mux.HandleFunc("GET /peer/v1/files", rm.guard("list", func(p Policy) bool { return p.Files }, rm.list))
	mux.HandleFunc("GET /peer/v1/file", rm.guard("read", func(p Policy) bool { return p.Files }, rm.read))
	mux.HandleFunc("POST /peer/v1/exec", rm.guard("exec", func(p Policy) bool { return p.Exec }, rm.exec))
	mux.HandleFunc("POST /peer/v1/delegate", rm.guard("delegate", func(p Policy) bool { return p.Delegate }, rm.delegate))
	mux.HandleFunc("GET /peer/v1/delegate/{id}", rm.guard("delegate-status", func(p Policy) bool { return p.Delegate }, rm.delegateStatus))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		c, ok := rm.Store.Authenticate(token)
		if !ok {
			http.Error(w, "unknown client", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		mux.ServeHTTP(w, r.WithContext(withClient(r.Context(), c)))
	})
}

// result is what a handler reports for the audit log.
type result struct {
	detail string
	err    error
}

// guard checks the client's policy for op, runs h and records the call.
func (rm *Service) guard(op string, allowed func(Policy) bool, h func(w http.ResponseWriter, r *http.Request, c Client) result) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := clientFrom(r.Context())
		start := time.Now()
		var res result
		if !allowed(c.Policy) {
			res.err = errNotAllowed
			http.Error(w, fmt.Sprintf("%s is not allowed for %q by this machine's settings", op, c.Name), http.StatusForbidden)
		} else {
			res = h(w, r, c)
		}
		e := AuditEntry{At: start, Client: c.Name, ClientID: c.ID, Op: op, Detail: res.detail, OK: res.err == nil, DurationMs: time.Since(start).Milliseconds()}
		if res.err != nil {
			e.Error = res.err.Error()
		}
		if rm.Audit != nil {
			if err := rm.Audit.Add(e); err != nil {
				log.Printf("peer audit: %v", err)
			}
		}
	}
}

var errNotAllowed = errors.New("not allowed by policy")

func (rm *Service) hello(w http.ResponseWriter, r *http.Request) {
	c := clientFrom(r.Context())
	host, _ := os.Hostname()
	writeJSON(w, http.StatusOK, map[string]any{"client": c.Name, "hostname": host, "version": rm.Version, "root": rm.Root, "policy": c.Policy})
}

func (rm *Service) status(w http.ResponseWriter, r *http.Request, c Client) result {
	var v any = map[string]any{}
	if rm.Status != nil {
		v = rm.Status()
	}
	writeJSON(w, http.StatusOK, v)
	return result{}
}

func (rm *Service) list(w http.ResponseWriter, r *http.Request, c Client) result {
	p := r.URL.Query().Get("path")
	full, err := rm.rooted(p)
	if err != nil {
		writeError(w, err)
		return result{p, err}
	}
	l, err := fsapi.ListDir(rm.Root, full, true)
	if err != nil {
		writeError(w, err)
		return result{p, err}
	}
	writeJSON(w, http.StatusOK, l)
	return result{detail: l.Path}
}

func (rm *Service) read(w http.ResponseWriter, r *http.Request, c Client) result {
	p := r.URL.Query().Get("path")
	full, err := rm.rooted(p)
	if err != nil {
		writeError(w, err)
		return result{p, err}
	}
	t, err := fsapi.ReadText(rm.Root, full, MaxReadFile)
	if err != nil {
		writeError(w, err)
		return result{p, err}
	}
	writeJSON(w, http.StatusOK, t)
	return result{detail: t.Path}
}

func (rm *Service) exec(w http.ResponseWriter, r *http.Request, c Client) result {
	var body struct {
		Command    string `json:"command"`
		Cwd        string `json:"cwd"`
		TimeoutSec int    `json:"timeoutSec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return result{err: err}
	}
	cmd := strings.TrimSpace(body.Command)
	if cmd == "" || len(cmd) > maxCommand {
		err := fmt.Errorf("command must be 1 to %d bytes", maxCommand)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return result{cmd, err}
	}
	dir, err := rm.dir(body.Cwd)
	if err != nil {
		writeError(w, err)
		return result{cmd, err}
	}
	timeout := DefaultExecTimeout
	if body.TimeoutSec > 0 {
		timeout = min(time.Duration(body.TimeoutSec)*time.Second, MaxExecTimeout)
	}
	res, err := runCommand(r.Context(), rm.Shell, dir, cmd, rm.Env, timeout, MaxExecOutput)
	detail := fmt.Sprintf("%s (in %s)", cmd, dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return result{detail, err}
	}
	writeJSON(w, http.StatusOK, res)
	return result{detail: fmt.Sprintf("%s -> exit %d", detail, res.ExitCode)}
}

func (rm *Service) delegate(w http.ResponseWriter, r *http.Request, c Client) result {
	var body struct {
		Cwd    string `json:"cwd"`
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return result{err: err}
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" || len(prompt) > maxPrompt {
		err := fmt.Errorf("prompt must be 1 to %d bytes", maxPrompt)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return result{err: err}
	}
	dir, err := rm.dir(body.Cwd)
	if err != nil {
		writeError(w, err)
		return result{prompt, err}
	}
	if rm.Delegator == nil {
		err := errors.New("delegation is not available")
		http.Error(w, err.Error(), http.StatusNotImplemented)
		return result{prompt, err}
	}
	id, err := rm.Delegator.Delegate(dir, prompt, c.Policy.DelegateMode, "依頼: "+c.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return result{prompt, err}
	}
	if err := rm.Store.AddDelegated(c.ID, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return result{prompt, err}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"session": id, "cwd": dir, "permissionMode": c.Policy.DelegateMode})
	return result{detail: fmt.Sprintf("%s (in %s, session %s)", prompt, dir, id)}
}

func (rm *Service) delegateStatus(w http.ResponseWriter, r *http.Request, c Client) result {
	id := r.PathValue("id")
	// 自分が依頼したセッションの結果だけを読める
	if !slices.Contains(c.Delegated, id) {
		http.Error(w, "not found", http.StatusNotFound)
		return result{id, fs.ErrNotExist}
	}
	st, err := rm.Delegator.DelegateStatus(id)
	if err != nil {
		writeError(w, err)
		return result{id, err}
	}
	writeJSON(w, http.StatusOK, st)
	return result{detail: id + " -> " + st.State}
}

// rooted makes a relative path relative to the root and rejects paths that
// are outside the root before touching the file system, so a client cannot
// learn whether files outside the root exist (404 vs 403). fsapi still
// resolves symlinks afterwards.
func (rm *Service) rooted(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(rm.Root, p)
	}
	p = filepath.Clean(p)
	if !fsapi.Within(rm.Root, p) {
		return "", fsapi.ErrOutsideRoot
	}
	return p, nil
}

// dir resolves a working directory inside the root ("" means the root).
func (rm *Service) dir(p string) (string, error) {
	full, err := rm.rooted(p)
	if err != nil {
		return "", err
	}
	d, err := fsapi.Resolve(rm.Root, full)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(d)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory", fsapi.ErrNotRegular, d)
	}
	return d, nil
}

func writeError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	switch {
	case errors.Is(err, fsapi.ErrOutsideRoot):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, fsapi.ErrNotRegular), errors.Is(err, fsapi.ErrBinary):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.As(err, &maxErr):
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
	case errors.Is(err, fs.ErrPermission):
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
