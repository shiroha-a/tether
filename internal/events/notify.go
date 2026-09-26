package events

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tether/internal/session"
)

// HookPayload is the subset of the Claude Code hook input that tether uses.
type HookPayload struct {
	SessionID     string `json:"session_id"`
	HookEventName string `json:"hook_event_name"`
	Message       string `json:"message"`
	Cwd           string `json:"cwd"`
	Source        string `json:"source"`
	// NotificationType is set on Notification events, e.g. "permission_prompt",
	// "elicitation_dialog", "idle_prompt" or "auth_success".
	NotificationType string `json:"notification_type"`
}

// Notification is published to UI clients and forwarded to Discord.
type Notification struct {
	Type    string    `json:"type"` // always "notify"
	Kind    string    `json:"kind"` // "stop" | "attention" | "schedule"
	Session string    `json:"session"`
	Label   string    `json:"label"`
	Cwd     string    `json:"cwd"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// Notifier handles hook callbacks and delivers notifications.
type Notifier struct {
	Hub      *Hub
	Sessions *session.Manager
	Discord  string
	HTTP     *http.Client
	// Debounce suppresses repeated Stop notifications for one session.
	Debounce time.Duration

	mu       sync.Mutex
	lastStop map[string]time.Time
	history  []Notification
}

// historySize is how many recent notifications are kept for the home screen.
const historySize = 50

// HandleHook serves POST /internal/hook?sid=&key=.
func (n *Notifier) HandleHook(w http.ResponseWriter, r *http.Request) {
	sid, key := r.URL.Query().Get("sid"), r.URL.Query().Get("key")
	s, err := n.Sessions.Get(sid)
	if err != nil {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	spec := s.Spec()
	if key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(spec.HookKey)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var p HookPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	// /clearや/compact後はClaude Code側のsession_idが変わるため、復元に使うIDを最新に追従させる
	if p.SessionID != "" && p.SessionID != spec.ClaudeSessionID {
		n.Sessions.Update(sid, func(sp *session.Spec) { sp.ClaudeSessionID = p.SessionID })
	}
	switch p.HookEventName {
	case "UserPromptSubmit":
		n.Sessions.SetActivity(sid, session.ActivityWorking, "")
	case "Stop":
		n.Sessions.SetActivity(sid, session.ActivityIdle, "")
		if n.shouldNotifyStop(sid) {
			n.Send(Notification{Kind: "stop", Session: sid, Label: label(spec), Cwd: spec.Cwd, Message: "応答が完了しました"})
		}
	case "Notification":
		switch notificationKind(p) {
		case notifyChoice:
			msg := p.Message
			if msg == "" {
				msg = "選択を待っています"
			}
			n.Sessions.SetActivity(sid, session.ActivityWaiting, msg)
			n.Send(Notification{Kind: "attention", Session: sid, Label: label(spec), Cwd: spec.Cwd, Message: msg})
		case notifyIdle:
			// 応答後にしばらく入力がないときの通知。完了の通知と重複するので、状態だけ完了にする
			n.Sessions.SetActivity(sid, session.ActivityIdle, "")
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

type notifyClass int

const (
	notifyIgnore notifyClass = iota
	notifyChoice             // the user must pick an option (permission prompt, dialog)
	notifyIdle               // Claude finished and is waiting for the next prompt
)

// notificationKind classifies a Notification hook. Older Claude Code versions
// do not send notification_type, so the message text is used as a fallback.
func notificationKind(p HookPayload) notifyClass {
	switch p.NotificationType {
	case "permission_prompt", "elicitation_dialog":
		return notifyChoice
	case "idle_prompt":
		return notifyIdle
	case "":
	default:
		// auth_success等、操作が不要な通知
		return notifyIgnore
	}
	msg := strings.ToLower(p.Message)
	if strings.Contains(msg, "waiting for your input") {
		return notifyIdle
	}
	return notifyChoice
}

func (n *Notifier) shouldNotifyStop(sid string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.lastStop == nil {
		n.lastStop = map[string]time.Time{}
	}
	now := time.Now()
	if last, ok := n.lastStop[sid]; ok && now.Sub(last) < n.Debounce {
		return false
	}
	n.lastStop[sid] = now
	return true
}

// Send publishes a notification to UI clients and Discord.
func (n *Notifier) Send(note Notification) {
	note.Type = "notify"
	if note.At.IsZero() {
		note.At = time.Now()
	}
	n.mu.Lock()
	n.history = append(n.history, note)
	if len(n.history) > historySize {
		n.history = append([]Notification(nil), n.history[len(n.history)-historySize:]...)
	}
	n.mu.Unlock()
	n.Hub.Publish(note)
	if n.Discord != "" {
		go n.postDiscord(note)
	}
}

// Recent returns up to historySize notifications, newest first.
func (n *Notifier) Recent() []Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Notification, len(n.history))
	for i, note := range n.history {
		out[len(n.history)-1-i] = note
	}
	return out
}

// HandleRecent serves GET /api/notifications.
func (n *Notifier) HandleRecent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(n.Recent())
}

func (n *Notifier) postDiscord(note Notification) {
	body, _ := json.Marshal(map[string]string{
		"content": fmt.Sprintf("**[%s]** %s\n`%s`", note.Label, note.Message, note.Cwd),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, n.Discord, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	client := n.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		log.Printf("discord notify: %v", err)
		return
	}
	res.Body.Close()
	if res.StatusCode >= 300 {
		log.Printf("discord notify: status %d", res.StatusCode)
	}
}

func label(sp session.Spec) string {
	if sp.Label != "" {
		return sp.Label
	}
	return filepath.Base(sp.Cwd)
}
