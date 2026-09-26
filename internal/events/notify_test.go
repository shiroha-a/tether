package events

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"tether/internal/session"
)

func setup(t *testing.T, discord string) (*Notifier, session.Spec, <-chan []byte) {
	t.Helper()
	dir := t.TempDir()
	m, err := session.NewManager(dir, func(session.Spec) *exec.Cmd { return exec.Command("cat") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	s, err := m.Create(session.Options{Cwd: dir, Label: "proj"})
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub()
	ch, cancel := hub.Subscribe()
	t.Cleanup(cancel)
	return &Notifier{Hub: hub, Sessions: m, Discord: discord, Debounce: time.Hour}, s.Spec(), ch
}

func hook(n *Notifier, sid, key, body string) int {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/hook?sid="+sid+"&key="+key, strings.NewReader(body))
	n.HandleHook(rec, req)
	return rec.Code
}

func next(t *testing.T, ch <-chan []byte) *Notification {
	t.Helper()
	select {
	case b := <-ch:
		var n Notification
		json.Unmarshal(b, &n)
		return &n
	case <-time.After(200 * time.Millisecond):
		return nil
	}
}

func TestHookAuth(t *testing.T) {
	n, sp, ch := setup(t, "")
	body := `{"hook_event_name":"Stop"}`
	if c := hook(n, sp.ID, "", body); c != http.StatusForbidden {
		t.Fatalf("empty key: %d", c)
	}
	if c := hook(n, sp.ID, "wrong", body); c != http.StatusForbidden {
		t.Fatalf("wrong key: %d", c)
	}
	if c := hook(n, "nope", sp.HookKey, body); c != http.StatusNotFound {
		t.Fatalf("unknown sid: %d", c)
	}
	if got := next(t, ch); got != nil {
		t.Fatalf("unauthenticated hook produced %+v", got)
	}
}

func TestStopNotificationDebounced(t *testing.T) {
	n, sp, ch := setup(t, "")
	if c := hook(n, sp.ID, sp.HookKey, `{"hook_event_name":"Stop"}`); c != http.StatusNoContent {
		t.Fatalf("status %d", c)
	}
	got := next(t, ch)
	if got == nil || got.Kind != "stop" || got.Label != "proj" || got.Session != sp.ID {
		t.Fatalf("notification = %+v", got)
	}
	hook(n, sp.ID, sp.HookKey, `{"hook_event_name":"Stop"}`)
	if got := next(t, ch); got != nil {
		t.Fatalf("second Stop within debounce produced %+v", got)
	}
	// Notificationはデバウンス対象外
	hook(n, sp.ID, sp.HookKey, `{"hook_event_name":"Notification","message":"Claude needs your permission"}`)
	if got := next(t, ch); got == nil || got.Kind != "attention" || got.Message != "Claude needs your permission" {
		t.Fatalf("attention = %+v", got)
	}
}

func TestHookTracksClaudeSessionID(t *testing.T) {
	n, sp, _ := setup(t, "")
	hook(n, sp.ID, sp.HookKey, `{"hook_event_name":"SessionStart","source":"clear","session_id":"new-id"}`)
	s, _ := n.Sessions.Get(sp.ID)
	if got := s.Spec().ClaudeSessionID; got != "new-id" {
		t.Fatalf("claude session id = %q", got)
	}
}

func TestDiscordForwarding(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
	}))
	defer srv.Close()
	n, sp, _ := setup(t, srv.URL)
	hook(n, sp.ID, sp.HookKey, `{"hook_event_name":"Stop"}`)
	select {
	case b := <-got:
		if !strings.Contains(b, "proj") || !strings.Contains(b, "応答が完了しました") {
			t.Fatalf("discord body = %s", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("discord webhook not called")
	}
}

func TestHookUpdatesActivity(t *testing.T) {
	n, sp, _ := setup(t, "")
	activity := func() string {
		s, _ := n.Sessions.Get(sp.ID)
		return s.Status().Activity
	}
	steps := []struct{ event, want string }{
		{"UserPromptSubmit", session.ActivityWorking},
		{"Notification", session.ActivityWaiting},
		{"UserPromptSubmit", session.ActivityWorking},
		{"Stop", session.ActivityIdle},
	}
	for _, st := range steps {
		if c := hook(n, sp.ID, sp.HookKey, `{"hook_event_name":"`+st.event+`"}`); c != http.StatusNoContent {
			t.Fatalf("%s: status %d", st.event, c)
		}
		if got := activity(); got != st.want {
			t.Fatalf("after %s: activity = %q, want %q", st.event, got, st.want)
		}
	}
	// 認証に失敗したhookでは状態を変えない
	hook(n, sp.ID, "wrong", `{"hook_event_name":"UserPromptSubmit"}`)
	if got := activity(); got != session.ActivityIdle {
		t.Fatalf("unauthenticated hook changed activity to %q", got)
	}
}

func TestActivityClearedOnExit(t *testing.T) {
	n, sp, _ := setup(t, "")
	hook(n, sp.ID, sp.HookKey, `{"hook_event_name":"UserPromptSubmit"}`)
	n.Sessions.Stop(sp.ID)
	s, _ := n.Sessions.Get(sp.ID)
	if st := s.Status(); st.Activity != "" || !st.ActivityAt.IsZero() {
		t.Fatalf("activity after exit = %q at %v", st.Activity, st.ActivityAt)
	}
	// 停止中のセッションには状態を設定しない（古いhookが遅れて届いた場合など）
	n.Sessions.SetActivity(sp.ID, session.ActivityWorking, "")
	if st := s.Status(); st.Activity != "" {
		t.Fatalf("stopped session got activity %q", st.Activity)
	}
}

func TestRecentNotifications(t *testing.T) {
	n, sp, _ := setup(t, "")
	n.Debounce = 0
	for i := 0; i < historySize+5; i++ {
		n.Send(Notification{Kind: "stop", Session: sp.ID, Message: strconv.Itoa(i)})
	}
	got := n.Recent()
	if len(got) != historySize {
		t.Fatalf("len = %d, want %d", len(got), historySize)
	}
	if got[0].Message != strconv.Itoa(historySize+4) || got[len(got)-1].Message != "5" {
		t.Fatalf("order: first=%q last=%q", got[0].Message, got[len(got)-1].Message)
	}
	rec := httptest.NewRecorder()
	n.HandleRecent(rec, httptest.NewRequest(http.MethodGet, "/api/notifications", nil))
	var body []Notification
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil || len(body) != historySize || body[0].Type != "notify" {
		t.Fatalf("handler: %v len=%d", err, len(body))
	}
}

func TestNotificationTypes(t *testing.T) {
	cases := []struct {
		name, body   string
		wantActivity string
		wantNotice   bool
	}{
		{"permission prompt", `{"hook_event_name":"Notification","notification_type":"permission_prompt","message":"Claude needs your permission to use Bash"}`, session.ActivityWaiting, true},
		{"elicitation dialog", `{"hook_event_name":"Notification","notification_type":"elicitation_dialog","message":"Choose an option"}`, session.ActivityWaiting, true},
		{"idle prompt", `{"hook_event_name":"Notification","notification_type":"idle_prompt","message":"Claude is waiting for your input"}`, session.ActivityIdle, false},
		{"auth success", `{"hook_event_name":"Notification","notification_type":"auth_success","message":"ok"}`, session.ActivityWorking, false},
		{"legacy idle text", `{"hook_event_name":"Notification","message":"Claude is waiting for your input"}`, session.ActivityIdle, false},
		{"legacy permission text", `{"hook_event_name":"Notification","message":"Claude needs your permission to use Write"}`, session.ActivityWaiting, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, sp, ch := setup(t, "")
			// 作業中から始めて、通知で状態がどう変わるかを見る
			hook(n, sp.ID, sp.HookKey, `{"hook_event_name":"UserPromptSubmit"}`)
			if code := hook(n, sp.ID, sp.HookKey, c.body); code != http.StatusNoContent {
				t.Fatalf("status %d", code)
			}
			s, _ := n.Sessions.Get(sp.ID)
			st := s.Status()
			if st.Activity != c.wantActivity {
				t.Fatalf("activity = %q, want %q", st.Activity, c.wantActivity)
			}
			// 選択待ちのときは、何を聞かれているかを画面に出せるよう本文を残す
			if c.wantActivity == session.ActivityWaiting && !strings.Contains(c.body, st.ActivityDetail) || c.wantActivity != session.ActivityWaiting && st.ActivityDetail != "" {
				t.Fatalf("activity detail = %q", st.ActivityDetail)
			}
			if c.wantActivity == session.ActivityWaiting && st.ActivityDetail == "" {
				t.Fatal("activity detail is empty while waiting")
			}
			got := next(t, ch)
			if (got != nil && got.Type == "notify") != c.wantNotice {
				t.Fatalf("notification = %+v, want notice %v", got, c.wantNotice)
			}
		})
	}
}

func TestAnsweredPromptSkipsLateNotification(t *testing.T) {
	n, sp, ch := setup(t, "")
	s, _ := n.Sessions.Get(sp.ID)
	activity := func() string { return s.Status().Activity }
	perm := `{"hook_event_name":"Notification","notification_type":"permission_prompt","message":"Claude needs your permission to use Bash"}`

	// 許可確認が出てすぐターミナルで答え、その後に通知が届く
	hook(n, sp.ID, sp.HookKey, `{"hook_event_name":"UserPromptSubmit"}`)
	hook(n, sp.ID, sp.HookKey, `{"hook_event_name":"PreToolUse"}`)
	time.Sleep(2 * time.Millisecond)
	s.Write([]byte("\r"))
	hook(n, sp.ID, sp.HookKey, perm)
	if got := activity(); got != session.ActivityWorking {
		t.Fatalf("late notification set activity %q", got)
	}
	for note := next(t, ch); note != nil; note = next(t, ch) {
		if note.Kind == "attention" {
			t.Fatalf("attention sent for an answered prompt: %+v", note)
		}
	}

	// 答える前に通知が届けば、待ちになり通知も送る
	time.Sleep(2 * time.Millisecond)
	hook(n, sp.ID, sp.HookKey, `{"hook_event_name":"PreToolUse"}`)
	hook(n, sp.ID, sp.HookKey, perm)
	if got := activity(); got != session.ActivityWaiting {
		t.Fatalf("unanswered prompt: activity %q", got)
	}
	found := false
	for note := next(t, ch); note != nil; note = next(t, ch) {
		found = found || note.Kind == "attention"
	}
	if !found {
		t.Fatal("no attention notification for an unanswered prompt")
	}
	// 待ちの間に次のツールが始まれば作業中に戻る
	hook(n, sp.ID, sp.HookKey, `{"hook_event_name":"PreToolUse"}`)
	if got := activity(); got != session.ActivityWorking {
		t.Fatalf("PreToolUse while waiting: %q", got)
	}
}
