package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"tether/internal/peer"
	"tether/internal/schedule"
	"tether/internal/session"
	"tether/internal/transcript"
)

// delegator starts sessions on this machine for peer clients.
type delegator struct{ s *Server }

func (d delegator) Delegate(dir, prompt, permissionMode, label string) (string, error) {
	sess, err := d.s.sessions.Create(session.Options{Kind: session.KindClaude, Cwd: dir, PermissionMode: permissionMode, Label: label})
	if err != nil {
		return "", err
	}
	id := sess.Spec().ID
	go func() {
		// 起動直後のTUIは入力を取りこぼすので、起動を待ってから送る
		time.Sleep(d.s.startupDelay)
		if err := sess.Write(schedule.EncodePrompt(prompt)); err != nil {
			log.Printf("delegate %s: %v", id, err)
			return
		}
		time.Sleep(d.s.enterDelay)
		sess.Write([]byte("\r"))
	}()
	return id, nil
}

func (d delegator) DelegateStatus(id string) (peer.DelegateStatus, error) {
	sess, err := d.s.sessions.Get(id)
	if err != nil {
		return peer.DelegateStatus{}, err
	}
	st := sess.Status()
	out := peer.DelegateStatus{Session: id, State: st.Activity, Detail: st.ActivityDetail}
	switch {
	case !st.Running:
		out.State = "stopped"
	case out.State == "":
		out.State = "starting"
	}
	if d.s.transcripts != nil {
		if path, ok := d.s.transcripts(st.ClaudeSessionID); ok {
			if items, _, err := transcript.ReadFrom(path, 0); err == nil {
				for i := len(items) - 1; i >= 0; i-- {
					if items[i].Kind == transcript.KindAssistant && items[i].Text != "" {
						out.Reply, out.ReplyAt = items[i].Text, items[i].At
						break
					}
				}
			}
		}
	}
	return out, nil
}

// remoteView is what the UI sees: tokens are never returned.
type remoteView struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	URL       string      `json:"url"`
	Policy    peer.Policy `json:"policy"`
	CreatedAt time.Time   `json:"createdAt"`
}

type clientView struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Policy     peer.Policy `json:"policy"`
	CreatedAt  time.Time   `json:"createdAt"`
	LastSeenAt time.Time   `json:"lastSeenAt,omitzero"`
}

func (s *Server) remoteState(w http.ResponseWriter, r *http.Request) {
	remotes := []remoteView{}
	for _, x := range s.peers.Remotes() {
		remotes = append(remotes, remoteView{x.ID, x.Name, x.URL, x.Policy, x.CreatedAt})
	}
	clients := []clientView{}
	for _, c := range s.peers.Clients() {
		clients = append(clients, clientView{c.ID, c.Name, c.Policy, c.CreatedAt, c.LastSeenAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"remotes": remotes, "clients": clients,
		"approvals": s.broker.Pending(), "grants": s.broker.Grants(),
		"delegateModes": peer.DelegateModes,
	})
}

func (s *Server) addRemote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		Code string `json:"code"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	u, token, err := peer.ParsePairingCode(body.Code)
	if peerErr(w, err) {
		return
	}
	// 登録前に接続とトークンを確かめ、相手が許可している操作を控えておく
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	hello, err := s.peerCaller.Hello(ctx, u, token)
	if err != nil {
		http.Error(w, "could not connect: "+err.Error(), http.StatusBadGateway)
		return
	}
	x, err := s.peers.AddRemote(body.Name, u, token, hello.Policy)
	if peerErr(w, err) {
		return
	}
	s.remoteChanged()
	writeJSON(w, http.StatusCreated, remoteView{x.ID, x.Name, x.URL, x.Policy, x.CreatedAt})
}

func (s *Server) refreshRemote(w http.ResponseWriter, r *http.Request) {
	x, ok := s.peers.RemoteByID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	hello, err := s.peerCaller.Hello(ctx, x.URL, x.Token)
	if err != nil {
		http.Error(w, "could not connect: "+err.Error(), http.StatusBadGateway)
		return
	}
	x, err = s.peers.SetRemotePolicy(x.ID, hello.Policy)
	if peerErr(w, err) {
		return
	}
	s.remoteChanged()
	writeJSON(w, http.StatusOK, remoteView{x.ID, x.Name, x.URL, x.Policy, x.CreatedAt})
}

func (s *Server) deleteRemote(w http.ResponseWriter, r *http.Request) {
	if peerErr(w, s.peers.DeleteRemote(r.PathValue("id"))) {
		return
	}
	s.remoteChanged()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) addClient(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string      `json:"name"`
		Policy peer.Policy `json:"policy"`
		// URL is how the other machine reaches this one (the page's origin).
		URL string `json:"url"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	c, token, err := s.peers.AddClient(body.Name, body.Policy)
	if peerErr(w, err) {
		return
	}
	s.remoteChanged()
	writeJSON(w, http.StatusCreated, map[string]any{
		"client": clientView{c.ID, c.Name, c.Policy, c.CreatedAt, c.LastSeenAt},
		"code":   peer.PairingCode(body.URL, token),
	})
}

func (s *Server) setClientPolicy(w http.ResponseWriter, r *http.Request) {
	var p peer.Policy
	if !readJSON(w, r, &p) {
		return
	}
	c, err := s.peers.SetPolicy(r.PathValue("id"), p)
	if peerErr(w, err) {
		return
	}
	s.remoteChanged()
	writeJSON(w, http.StatusOK, clientView{c.ID, c.Name, c.Policy, c.CreatedAt, c.LastSeenAt})
}

func (s *Server) deleteClient(w http.ResponseWriter, r *http.Request) {
	if peerErr(w, s.peers.DeleteClient(r.PathValue("id"))) {
		return
	}
	s.remoteChanged()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request) {
	var d peer.Decision
	if !readJSON(w, r, &d) {
		return
	}
	if !s.broker.Decide(r.PathValue("id"), d) {
		http.Error(w, "this request is no longer waiting", http.StatusGone)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeGrant(w http.ResponseWriter, r *http.Request) {
	if !s.broker.Revoke(r.PathValue("id")) {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) peerAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := s.peerAuditLog.Recent(200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []peer.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) remoteChanged() {
	s.hub.Publish(map[string]any{"type": "remote.changed"})
}

// peerErr writes the response for a peer store error and reports whether there was one.
func peerErr(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, peer.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, peer.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		log.Printf("peers: %v", err)
		http.Error(w, "failed to save", http.StatusInternalServerError)
	}
	return true
}

// mcp handles MCP requests from `tether mcp`, authenticated like hooks with
// the session's key. Approvals are never accepted here.
func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	sess, err := s.sessions.Get(r.URL.Query().Get("sid"))
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	spec := sess.Spec()
	if spec.HookKey == "" || r.URL.Query().Get("key") != spec.HookKey || spec.IsShell() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		Method    string          `json:"method"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	switch body.Method {
	case "tools/list":
		writeJSON(w, http.StatusOK, map[string]any{"tools": peer.ToolList})
	case "tools/call":
		res := s.peerTools.Call(r.Context(), spec.ID, session.DisplayLabel(spec), body.Name, body.Arguments)
		writeJSON(w, http.StatusOK, map[string]any{"text": res.Text, "isError": res.IsError})
	default:
		http.Error(w, "unknown method", http.StatusBadRequest)
	}
}
