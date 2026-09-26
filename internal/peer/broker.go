package peer

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Approval is a pending request from a Claude Code session to use a remote.
type Approval struct {
	ID      string `json:"id"`
	Session string `json:"session"`
	// SessionLabel is the session's display name.
	SessionLabel string `json:"sessionLabel"`
	Machine      string `json:"machine"`
	Op           string `json:"op"`
	// Detail is what will be done (command, path, prompt).
	Detail string `json:"detail"`
	// Read is true for read-only operations, which may be granted for a while.
	Read      bool      `json:"read"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Grant allows read-only calls from a session to a machine until ExpiresAt.
type Grant struct {
	ID        string    `json:"id"`
	Session   string    `json:"session"`
	Machine   string    `json:"machine"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Decision is the user's answer to an approval.
type Decision struct {
	Allow bool `json:"allow"`
	// GrantMinutes, for read-only requests, also allows further reads from the
	// same session to the same machine for this long.
	GrantMinutes int `json:"grantMinutes"`
}

// Outcome is the result of Ask.
type Outcome int

const (
	Allowed Outcome = iota
	Denied
	TimedOut
	Canceled
)

// OpLabel is the Japanese name of an operation, for notifications.
func OpLabel(op string) string {
	switch op {
	case "status":
		return "サーバの状態の取得"
	case "list":
		return "フォルダの一覧"
	case "read":
		return "ファイルの読み取り"
	case "exec":
		return "コマンドの実行"
	case "delegate":
		return "作業の依頼"
	case "delegate-status":
		return "依頼の結果の確認"
	}
	return op
}

// MaxGrant caps how long a read grant may last.
const MaxGrant = 60 * time.Minute

type pending struct {
	Approval
	ch chan Decision
}

// Broker holds approvals waiting for the user and the active read grants.
// Approvals are answered through the UI API, never through the MCP channel.
type Broker struct {
	// Timeout is how long to wait for an answer (the request is denied after).
	Timeout time.Duration
	// OnChange is called when approvals or grants change (for UI events).
	OnChange func()
	// OnRequest is called for each new approval (for notifications).
	OnRequest func(Approval)

	mu      sync.Mutex
	pending map[string]*pending
	grants  []Grant
	now     func() time.Time
}

// NewBroker returns a broker that waits timeout for answers.
func NewBroker(timeout time.Duration) *Broker {
	return &Broker{Timeout: timeout, pending: map[string]*pending{}, now: time.Now}
}

// Ask waits for the user to approve a. Read-only requests covered by a grant
// are allowed at once.
func (b *Broker) Ask(ctx context.Context, a Approval) Outcome {
	b.mu.Lock()
	now := b.now()
	if a.Read && b.grantedLocked(a.Session, a.Machine, now) {
		b.mu.Unlock()
		return Allowed
	}
	a.ID = randomHex(8)
	a.CreatedAt, a.ExpiresAt = now, now.Add(b.Timeout)
	p := &pending{Approval: a, ch: make(chan Decision, 1)}
	b.pending[a.ID] = p
	b.mu.Unlock()
	b.changed()
	if b.OnRequest != nil {
		b.OnRequest(a)
	}

	timer := time.NewTimer(b.Timeout)
	defer timer.Stop()
	out := TimedOut
	select {
	case d := <-p.ch:
		out = Denied
		if d.Allow {
			out = Allowed
		}
	case <-timer.C:
	case <-ctx.Done():
		out = Canceled
	}
	b.mu.Lock()
	delete(b.pending, a.ID)
	b.mu.Unlock()
	b.changed()
	return out
}

// Decide answers a pending approval. It reports false if it is no longer pending.
func (b *Broker) Decide(id string, d Decision) bool {
	b.mu.Lock()
	p, ok := b.pending[id]
	if ok {
		delete(b.pending, id)
		// まとめての許可は読み取りだけ。コマンド実行や依頼は毎回確認する
		if d.Allow && p.Read && d.GrantMinutes > 0 {
			dur := min(time.Duration(d.GrantMinutes)*time.Minute, MaxGrant)
			b.grants = append(b.grants, Grant{ID: randomHex(8), Session: p.Session, Machine: p.Machine, ExpiresAt: b.now().Add(dur)})
		}
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	p.ch <- d
	b.changed()
	return true
}

// Pending returns approvals waiting for an answer, oldest first.
func (b *Broker) Pending() []Approval {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Approval, 0, len(b.pending))
	for _, p := range b.pending {
		out = append(out, p.Approval)
	}
	slices.SortFunc(out, func(x, y Approval) int { return x.CreatedAt.Compare(y.CreatedAt) })
	return out
}

// Grants returns the active grants.
func (b *Broker) Grants() []Grant {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.grants = slices.DeleteFunc(b.grants, func(g Grant) bool { return !now.Before(g.ExpiresAt) })
	return append([]Grant{}, b.grants...)
}

// Revoke removes a grant.
func (b *Broker) Revoke(id string) bool {
	b.mu.Lock()
	n := len(b.grants)
	b.grants = slices.DeleteFunc(b.grants, func(g Grant) bool { return g.ID == id })
	ok := len(b.grants) != n
	b.mu.Unlock()
	if ok {
		b.changed()
	}
	return ok
}

// DropSession removes the grants of a stopped or deleted session.
func (b *Broker) DropSession(session string) {
	b.mu.Lock()
	n := len(b.grants)
	b.grants = slices.DeleteFunc(b.grants, func(g Grant) bool { return g.Session == session })
	ok := len(b.grants) != n
	b.mu.Unlock()
	if ok {
		b.changed()
	}
}

func (b *Broker) grantedLocked(session, machine string, now time.Time) bool {
	return slices.ContainsFunc(b.grants, func(g Grant) bool {
		return g.Session == session && g.Machine == machine && now.Before(g.ExpiresAt)
	})
}

func (b *Broker) changed() {
	if b.OnChange != nil {
		b.OnChange()
	}
}
