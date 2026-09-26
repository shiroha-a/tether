package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeCred(t *testing.T, token string, expires time.Time) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".credentials.json")
	b, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": token, "expiresAt": expires.UnixMilli(), "subscriptionType": "max",
	}})
	os.WriteFile(p, b, 0o600)
	return p
}

func TestFetchAndCache(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("anthropic-beta") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"five_hour":{"utilization":42.5,"resets_at":"2026-09-25T15:00:00Z"},"seven_day":{"utilization":10,"resets_at":null},"seven_day_opus":null,"extra":"x"}`))
	}))
	defer srv.Close()
	c := &Client{CredentialsPath: writeCred(t, "tok", time.Now().Add(time.Hour)), Endpoint: srv.URL, TTL: time.Minute}

	s := c.Get(context.Background(), false)
	if !s.Available || s.Plan != "max" {
		t.Fatalf("snapshot = %+v", s)
	}
	fh := s.Windows["five_hour"]
	if fh == nil || fh.Utilization != 42.5 || fh.ResetsAt == nil || fh.ResetsAt.Hour() != 15 {
		t.Fatalf("five_hour = %+v", fh)
	}
	if _, ok := s.Windows["seven_day_opus"]; ok {
		t.Fatal("null window should be skipped")
	}
	if _, ok := s.Windows["extra"]; ok {
		t.Fatal("non-window field should be skipped")
	}
	c.Get(context.Background(), false)
	if calls.Load() != 1 {
		t.Fatalf("cache miss: %d calls", calls.Load())
	}
	c.Get(context.Background(), true)
	if calls.Load() != 2 {
		t.Fatalf("force did not refetch: %d calls", calls.Load())
	}
}

func TestErrorsDoNotLeakToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := &Client{CredentialsPath: writeCred(t, "supersecret", time.Now().Add(time.Hour)), Endpoint: srv.URL, TTL: time.Minute}
	s := c.Get(context.Background(), false)
	if s.Available || !strings.Contains(s.Error, "500") || strings.Contains(s.Error, "supersecret") {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestExpiredTokenNotSent(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Store(true) }))
	defer srv.Close()
	c := &Client{CredentialsPath: writeCred(t, "tok", time.Now().Add(-time.Minute)), Endpoint: srv.URL, TTL: time.Minute}
	if s := c.Get(context.Background(), false); s.Available {
		t.Fatalf("expired token reported available: %+v", s)
	}
	if called.Load() {
		t.Fatal("expired token was sent to the API")
	}
}

func TestMissingCredentials(t *testing.T) {
	c := &Client{CredentialsPath: "/nonexistent/.credentials.json", Endpoint: "http://127.0.0.1:1", TTL: time.Minute}
	if s := c.Get(context.Background(), false); s.Available || s.Error == "" {
		t.Fatalf("snapshot = %+v", s)
	}
}
