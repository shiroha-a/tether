// Package peer connects tether instances on different machines. A machine
// ("remote", seen from the caller) lets another one ("client") use a limited
// set of operations, capped by a per-client policy, and every call made
// through Claude Code must be approved by the user on the calling machine.
package peer

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Errors returned by Store.
var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
)

const (
	maxNameRunes = 40
	maxPeers     = 50
	pairPrefix   = "tether-pair:"
)

// Policy is what a client may do on this machine. Exec and Delegate are off
// unless the user turns them on.
type Policy struct {
	Status   bool `json:"status"`
	Files    bool `json:"files"`
	Exec     bool `json:"exec"`
	Delegate bool `json:"delegate"`
	// DelegateMode is the permission mode of sessions created by Delegate.
	DelegateMode string `json:"delegateMode,omitempty"`
}

// DelegateModes are the permission modes a policy may give delegated sessions.
// 他のマシンから確認なしで何でも実行できてしまうので、bypassPermissionsは選べない
var DelegateModes = []string{"default", "acceptEdits", "plan", "auto"}

func checkPolicy(p Policy) (Policy, error) {
	if p.DelegateMode == "" {
		p.DelegateMode = "default"
	}
	if !slices.Contains(DelegateModes, p.DelegateMode) {
		return Policy{}, fmt.Errorf("%w: delegate mode %q is not allowed", ErrInvalid, p.DelegateMode)
	}
	return p, nil
}

// DefaultPolicy allows reading only.
func DefaultPolicy() Policy { return Policy{Status: true, Files: true, DelegateMode: "default"} }

// Client is a machine allowed to call this one.
type Client struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	TokenHash  string    `json:"tokenHash"`
	Policy     Policy    `json:"policy"`
	CreatedAt  time.Time `json:"createdAt"`
	LastSeenAt time.Time `json:"lastSeenAt,omitzero"`
	// Delegated lists sessions this client created with Delegate; it may only
	// read the results of these.
	Delegated []string `json:"delegated,omitempty"`
}

// Remote is a machine this one can call.
type Remote struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	URL   string `json:"url"`
	Token string `json:"token"`
	// Policy is what the remote reported at pairing time (informational;
	// the remote enforces its own copy).
	Policy    Policy    `json:"policy"`
	CreatedAt time.Time `json:"createdAt"`
}

type file struct {
	Clients []Client `json:"clients"`
	Remotes []Remote `json:"remotes"`
}

// Store persists clients and remotes in <dataDir>/peers.json (0600).
type Store struct {
	mu   sync.Mutex
	data file
	path string
	now  func() time.Time
	// seenSaved is when each client's LastSeenAt was last written to disk.
	seenSaved map[string]time.Time
}

// NewStore loads peers from dataDir.
func NewStore(dataDir string) (*Store, error) {
	s := &Store{path: filepath.Join(dataDir, "peers.json"), now: time.Now, seenSaved: map[string]time.Time{}}
	b, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.data); err != nil {
			return nil, fmt.Errorf("parse %s: %w", s.path, err)
		}
	}
	return s, nil
}

// Clients returns the machines allowed to call this one.
func (s *Store) Clients() []Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Client{}, s.data.Clients...)
}

// Remotes returns the machines this one can call.
func (s *Store) Remotes() []Remote {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Remote{}, s.data.Remotes...)
}

// Remote finds a remote by name.
func (s *Store) Remote(name string) (Remote, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.data.Remotes, func(r Remote) bool { return r.Name == name })
	if i < 0 {
		return Remote{}, false
	}
	return s.data.Remotes[i], true
}

// RemoteByID finds a remote by id.
func (s *Store) RemoteByID(id string) (Remote, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.data.Remotes, func(r Remote) bool { return r.ID == id })
	if i < 0 {
		return Remote{}, false
	}
	return s.data.Remotes[i], true
}

// SetRemotePolicy records the policy a remote reported (informational).
func (s *Store) SetRemotePolicy(id string, p Policy) (Remote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.data.Remotes, func(r Remote) bool { return r.ID == id })
	if i < 0 {
		return Remote{}, ErrNotFound
	}
	next := s.data
	next.Remotes = slices.Clone(s.data.Remotes)
	next.Remotes[i].Policy = p
	if err := s.commitLocked(next); err != nil {
		return Remote{}, err
	}
	return next.Remotes[i], nil
}

// AddClient registers a client and returns it with its token. The token is
// shown once; only its hash is stored.
func (s *Store) AddClient(name string, p Policy) (Client, string, error) {
	name, err := checkName(name)
	if err != nil {
		return Client{}, "", err
	}
	if p, err = checkPolicy(p); err != nil {
		return Client{}, "", err
	}
	token := randomHex(32)
	c := Client{ID: randomHex(8), Name: name, TokenHash: hashToken(token), Policy: p, CreatedAt: s.now()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Clients) >= maxPeers {
		return Client{}, "", fmt.Errorf("%w: too many clients", ErrInvalid)
	}
	if slices.ContainsFunc(s.data.Clients, func(x Client) bool { return x.Name == name }) {
		return Client{}, "", fmt.Errorf("%w: a client named %q already exists", ErrInvalid, name)
	}
	next := s.data
	next.Clients = append(slices.Clone(s.data.Clients), c)
	if err := s.commitLocked(next); err != nil {
		return Client{}, "", err
	}
	return c, token, nil
}

// SetPolicy replaces a client's policy.
func (s *Store) SetPolicy(id string, p Policy) (Client, error) {
	p, err := checkPolicy(p)
	if err != nil {
		return Client{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.data.Clients, func(c Client) bool { return c.ID == id })
	if i < 0 {
		return Client{}, ErrNotFound
	}
	next := s.data
	next.Clients = slices.Clone(s.data.Clients)
	next.Clients[i].Policy = p
	if err := s.commitLocked(next); err != nil {
		return Client{}, err
	}
	return next.Clients[i], nil
}

// maxDelegated caps Client.Delegated; the oldest ids are forgotten first.
const maxDelegated = 100

// AddDelegated records a session created by a client's Delegate call.
func (s *Store) AddDelegated(clientID, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.data.Clients, func(c Client) bool { return c.ID == clientID })
	if i < 0 {
		return ErrNotFound
	}
	next := s.data
	next.Clients = slices.Clone(s.data.Clients)
	d := append(slices.Clone(next.Clients[i].Delegated), sessionID)
	if len(d) > maxDelegated {
		d = d[len(d)-maxDelegated:]
	}
	next.Clients[i].Delegated = d
	return s.commitLocked(next)
}

// DeleteClient removes a client; its token stops working immediately.
func (s *Store) DeleteClient(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.data.Clients, func(c Client) bool { return c.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	next := s.data
	next.Clients = slices.Delete(slices.Clone(s.data.Clients), i, i+1)
	return s.commitLocked(next)
}

// Authenticate finds the client that owns token and records when it was seen.
func (s *Store) Authenticate(token string) (Client, bool) {
	h := hashToken(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.data.Clients {
		if subtle.ConstantTimeCompare([]byte(c.TokenHash), []byte(h)) == 1 {
			now := s.now()
			// 最終接続時刻は表示用。呼び出しのたびに書き込まないよう、保存は1分に1回まで
			s.data.Clients[i].LastSeenAt = now
			if last, ok := s.seenSaved[c.ID]; !ok || now.Sub(last) >= time.Minute {
				if s.commitLocked(s.data) == nil {
					s.seenSaved[c.ID] = now
				}
			}
			return s.data.Clients[i], true
		}
	}
	return Client{}, false
}

// AddRemote registers a remote reached at rawURL with token.
func (s *Store) AddRemote(name, rawURL, token string, p Policy) (Remote, error) {
	name, err := checkName(name)
	if err != nil {
		return Remote{}, err
	}
	u, err := checkURL(rawURL)
	if err != nil {
		return Remote{}, err
	}
	if token == "" {
		return Remote{}, fmt.Errorf("%w: token is empty", ErrInvalid)
	}
	r := Remote{ID: randomHex(8), Name: name, URL: u, Token: token, Policy: p, CreatedAt: s.now()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Remotes) >= maxPeers {
		return Remote{}, fmt.Errorf("%w: too many remotes", ErrInvalid)
	}
	if slices.ContainsFunc(s.data.Remotes, func(x Remote) bool { return x.Name == name }) {
		return Remote{}, fmt.Errorf("%w: a remote named %q already exists", ErrInvalid, name)
	}
	next := s.data
	next.Remotes = append(slices.Clone(s.data.Remotes), r)
	if err := s.commitLocked(next); err != nil {
		return Remote{}, err
	}
	return r, nil
}

// DeleteRemote removes a remote.
func (s *Store) DeleteRemote(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.data.Remotes, func(r Remote) bool { return r.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	next := s.data
	next.Remotes = slices.Delete(slices.Clone(s.data.Remotes), i, i+1)
	return s.commitLocked(next)
}

// commitLocked writes data to disk and only then makes it current.
func (s *Store) commitLocked(next file) error {
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.data = next
	return nil
}

// PairingCode encodes a remote's URL and token for pasting on the calling machine.
func PairingCode(remoteURL, token string) string {
	b, _ := json.Marshal(map[string]string{"url": remoteURL, "token": token})
	return pairPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// ParsePairingCode decodes a code made by PairingCode.
func ParsePairingCode(code string) (remoteURL, token string, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(code), pairPrefix)
	if !ok {
		return "", "", fmt.Errorf("%w: not a tether pairing code", ErrInvalid)
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return "", "", fmt.Errorf("%w: broken pairing code", ErrInvalid)
	}
	var v struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.URL == "" || v.Token == "" {
		return "", "", fmt.Errorf("%w: broken pairing code", ErrInvalid)
	}
	return v.URL, v.Token, nil
}

func checkName(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", fmt.Errorf("%w: name is empty", ErrInvalid)
	case utf8.RuneCountInString(name) > maxNameRunes:
		return "", fmt.Errorf("%w: name is too long", ErrInvalid)
	case strings.ContainsFunc(name, unicode.IsControl):
		return "", fmt.Errorf("%w: name must not contain control characters", ErrInvalid)
	}
	return name, nil
}

// checkURL accepts https URLs, and http only for loopback (local testing).
// トークンを平文で流さないよう、tailscale serve等のHTTPSを前提にする
func checkURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: URL must look like https://host[:port]", ErrInvalid)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" && h != "::1" {
			return "", fmt.Errorf("%w: http is only allowed for localhost; use https (tailscale serve)", ErrInvalid)
		}
	default:
		return "", fmt.Errorf("%w: URL must start with https://", ErrInvalid)
	}
	return u.Scheme + "://" + u.Host, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
