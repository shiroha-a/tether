package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"tether/internal/guard"
	"tether/internal/session"
)

type clientMsg struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// terminal serves GET /ws/sessions/{id}: it resumes the session if needed,
// replays buffered output and then relays input/output both ways.
func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Acceptでも同じ検証をするが、それより前に停止中セッションを再開（プロセス起動）してしまうので先に弾く
	if !guard.SameOrigin(r) {
		http.Error(w, "cross-origin websocket", http.StatusForbidden)
		return
	}
	sess, err := s.sessions.Ensure(id)
	if errors.Is(err, session.ErrNotFound) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Originの検証はcoder/websocketの既定（Originのホストがリクエストのホストと一致すること）に任せる
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4 << 20)

	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	client, replay := sess.Attach(clamp(cols), clamp(rows))
	defer func() { sess.Detach(client) }()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// ブラウザ側で過去の画面を消してから再描画させるため、リプレイの前にリセットを送る
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"replay"}`)); err != nil {
		return
	}
	// リプレイは現在のptyの幅で描かれた出力なので、受け手が先に同じ幅に合わせられるようサイズを先に送る
	st := sess.Status()
	if err := conn.Write(ctx, websocket.MessageText, mustJSON(map[string]any{"type": "state", "running": st.Running, "exitCode": st.ExitCode, "cols": st.Cols, "rows": st.Rows})); err != nil {
		return
	}
	if len(replay) > 0 {
		if err := conn.Write(ctx, websocket.MessageBinary, replay); err != nil {
			return
		}
	}

	go func() {
		defer cancel()
		ping := time.NewTicker(30 * time.Second)
		defer ping.Stop()
		for {
			select {
			case f, ok := <-client.Out:
				if !ok {
					// 遅いクライアントとして切断された、またはセッションが削除された
					conn.Close(websocket.StatusGoingAway, "detached")
					return
				}
				typ := websocket.MessageText
				if f.Binary {
					typ = websocket.MessageBinary
				}
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Write(wctx, typ, f.Data)
				wcancel()
				if err != nil {
					return
				}
			case <-ping.C:
				pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Ping(pctx)
				pcancel()
				if err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var m clientMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "input":
			if err := sess.Write([]byte(m.Data)); err != nil {
				conn.Write(ctx, websocket.MessageText, mustJSON(map[string]any{"type": "error", "message": err.Error()}))
			}
		case "resize":
			sess.Resize(client, clamp(m.Cols), clamp(m.Rows))
		case "restart":
			if _, err := s.sessions.Ensure(id); err != nil {
				log.Printf("[session %s] restart: %v", id, err)
				conn.Write(ctx, websocket.MessageText, mustJSON(map[string]any{"type": "error", "message": err.Error()}))
			}
		}
	}
}

// events serves GET /ws/events, streaming notifications and session changes.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ch, cancel := s.hub.Subscribe()
	defer cancel()
	// 読み取り側を回さないとclose/pingを処理できないため、受信は読み捨てる
	ctx := conn.CloseRead(r.Context())
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case b, ok := <-ch:
			if !ok {
				return
			}
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Write(wctx, websocket.MessageText, b)
			wcancel()
			if err != nil {
				return
			}
		case <-ping.C:
			pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// clamp keeps terminal dimensions in a sane range; 0 means "unknown".
func clamp(n int) int {
	if n <= 0 {
		return 0
	}
	return max(10, min(n, 1000))
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
