// Package usage fetches Claude subscription usage via the OAuth usage endpoint
// that Claude Code itself uses. The endpoint is undocumented and may change.
package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// DefaultEndpoint is the OAuth usage API.
const DefaultEndpoint = "https://api.anthropic.com/api/oauth/usage"

// Window is one rate-limit window.
type Window struct {
	Utilization float64    `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
}

// Snapshot is the response returned to the UI.
type Snapshot struct {
	Available bool               `json:"available"`
	Error     string             `json:"error,omitempty"`
	Plan      string             `json:"plan,omitempty"`
	Windows   map[string]*Window `json:"windows,omitempty"`
	FetchedAt time.Time          `json:"fetchedAt"`
}

// Client fetches and caches usage snapshots.
type Client struct {
	CredentialsPath string
	Endpoint        string
	HTTP            *http.Client
	TTL             time.Duration

	mu     sync.Mutex
	cached *Snapshot
}

type credentials struct {
	ClaudeAiOauth struct {
		AccessToken      string `json:"accessToken"`
		ExpiresAt        int64  `json:"expiresAt"`
		SubscriptionType string `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

// Get returns a cached snapshot unless it is stale or force is set.
func (c *Client) Get(ctx context.Context, force bool) Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.cached != nil && time.Since(c.cached.FetchedAt) < c.TTL {
		return *c.cached
	}
	snap := c.fetch(ctx)
	// 失敗時に前回の成功値を上書きすると表示が消えるので、失敗は短時間だけキャッシュする
	if snap.Available || c.cached == nil || !c.cached.Available {
		c.cached = &snap
	}
	return snap
}

func (c *Client) fetch(ctx context.Context) Snapshot {
	now := time.Now()
	fail := func(err error) Snapshot { return Snapshot{Error: err.Error(), FetchedAt: now} }

	b, err := os.ReadFile(c.CredentialsPath)
	if err != nil {
		return fail(errors.New("Claude Codeの認証情報が見つかりません"))
	}
	var cred credentials
	if err := json.Unmarshal(b, &cred); err != nil || cred.ClaudeAiOauth.AccessToken == "" {
		return fail(errors.New("Claude Codeの認証情報を読めません"))
	}
	if exp := cred.ClaudeAiOauth.ExpiresAt; exp > 0 && time.UnixMilli(exp).Before(now) {
		// トークンの更新はClaude Code自身に任せる（ここでrefreshすると、Claude Code側が持つトークンと食い違うため）
		return fail(errors.New("アクセストークンの期限切れ（Claude Codeを一度使うと更新されます）"))
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.Endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+cred.ClaudeAiOauth.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return fail(fmt.Errorf("usage API: %w", err))
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("usage API: status %d", res.StatusCode))
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return fail(errors.New("usage API: unexpected response"))
	}
	windows := map[string]*Window{}
	for k, v := range raw {
		var w Window
		// five_hour/seven_day等の{utilization,resets_at}形のフィールドだけを拾い、未知の形は無視する
		if json.Unmarshal(v, &w) == nil && string(v) != "null" && hasKey(v, "utilization") {
			windows[k] = &w
		}
	}
	return Snapshot{Available: true, Plan: cred.ClaudeAiOauth.SubscriptionType, Windows: windows, FetchedAt: now}
}

func hasKey(v json.RawMessage, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(v, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// Handler serves GET /api/usage.
func (c *Client) Handler(w http.ResponseWriter, r *http.Request) {
	snap := c.Get(r.Context(), r.URL.Query().Get("force") == "1")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snap)
}
