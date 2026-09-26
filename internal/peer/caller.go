package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"tether/internal/fsapi"
)

// Hello is what a remote reports about itself and the caller's policy.
type Hello struct {
	Client   string `json:"client"`
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
	Root     string `json:"root"`
	Policy   Policy `json:"policy"`
}

// Caller makes requests to remotes.
type Caller struct {
	// HTTP is the client to use; nil means a client with no overall timeout
	// (each call sets its own deadline).
	HTTP *http.Client
}

// RemoteError is a non-2xx response from a remote.
type RemoteError struct {
	Status  int
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("remote returned %d: %s", e.Status, e.Message)
}

const callTimeout = 30 * time.Second

func (c *Caller) do(ctx context.Context, base, token, method, path string, q url.Values, body, out any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 2000))
		return &RemoteError{Status: res.StatusCode, Message: strings.TrimSpace(string(msg))}
	}
	// 接続先から返る内容は信用しないので、大きすぎる応答は読まない
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
}

// Hello checks the connection and token, and returns the caller's policy.
func (c *Caller) Hello(ctx context.Context, base, token string) (Hello, error) {
	var h Hello
	err := c.do(ctx, base, token, http.MethodGet, "/peer/v1/hello", nil, nil, &h, callTimeout)
	return h, err
}

// Status returns the remote's host status (load, memory, disk, uptime).
func (c *Caller) Status(ctx context.Context, r Remote) (map[string]any, error) {
	var v map[string]any
	err := c.do(ctx, r.URL, r.Token, http.MethodGet, "/peer/v1/status", nil, nil, &v, callTimeout)
	return v, err
}

// List lists a directory on the remote.
func (c *Caller) List(ctx context.Context, r Remote, path string) (fsapi.Listing, error) {
	var l fsapi.Listing
	err := c.do(ctx, r.URL, r.Token, http.MethodGet, "/peer/v1/files", url.Values{"path": {path}}, nil, &l, callTimeout)
	return l, err
}

// Read reads a text file on the remote.
func (c *Caller) Read(ctx context.Context, r Remote, path string) (fsapi.TextFile, error) {
	var t fsapi.TextFile
	err := c.do(ctx, r.URL, r.Token, http.MethodGet, "/peer/v1/file", url.Values{"path": {path}}, nil, &t, callTimeout)
	return t, err
}

// Exec runs a command on the remote.
func (c *Caller) Exec(ctx context.Context, r Remote, command, cwd string, timeoutSec int) (ExecResult, error) {
	var res ExecResult
	wait := DefaultExecTimeout
	if timeoutSec > 0 {
		wait = min(time.Duration(timeoutSec)*time.Second, MaxExecTimeout)
	}
	body := map[string]any{"command": command, "cwd": cwd, "timeoutSec": timeoutSec}
	err := c.do(ctx, r.URL, r.Token, http.MethodPost, "/peer/v1/exec", nil, body, &res, wait+callTimeout)
	return res, err
}

// Delegated is the response to Delegate.
type Delegated struct {
	Session        string `json:"session"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permissionMode"`
}

// Delegate asks the remote to start a Claude Code session with prompt.
func (c *Caller) Delegate(ctx context.Context, r Remote, cwd, prompt string) (Delegated, error) {
	var d Delegated
	err := c.do(ctx, r.URL, r.Token, http.MethodPost, "/peer/v1/delegate", nil, map[string]string{"cwd": cwd, "prompt": prompt}, &d, callTimeout)
	return d, err
}

// DelegateStatus returns the state and last reply of a delegated session.
func (c *Caller) DelegateStatus(ctx context.Context, r Remote, session string) (DelegateStatus, error) {
	var st DelegateStatus
	err := c.do(ctx, r.URL, r.Token, http.MethodGet, "/peer/v1/delegate/"+url.PathEscape(session), nil, nil, &st, callTimeout)
	return st, err
}
