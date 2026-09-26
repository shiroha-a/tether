// Command tether is a web UI for running and sharing Claude Code sessions.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tether/internal/config"
	"tether/internal/events"
	"tether/internal/guard"
	"tether/internal/listen"
	"tether/internal/netallow"
	"tether/internal/peer"
	"tether/internal/schedule"
	"tether/internal/server"
	"tether/internal/session"
	"tether/internal/snippets"
	"tether/internal/usage"
	"tether/web"
)

var version = "dev"

// repository is the public source repository shown in the about dialog.
const repository = "https://github.com/shiroha-a/tether"

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		if err := serve(); err != nil {
			log.Fatal(err)
		}
	case "hook":
		hook(os.Args[2:])
	case "mcp":
		if err := mcpServe(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	case "version", "--version", "-v":
		fmt.Println("tether", version)
	default:
		fmt.Fprintf(os.Stderr, "usage: tether [serve|hook|mcp|version]\n")
		os.Exit(2)
	}
}

// hook is invoked by Claude Code hooks. It forwards the hook input to the
// server and always exits 0 so that a stopped server never blocks Claude Code.
func hook(args []string) {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	u := fs.String("url", "", "hook endpoint")
	sid := fs.String("sid", "", "tether session id")
	key := fs.String("key", "", "hook key")
	if fs.Parse(args) != nil || *u == "" {
		return
	}
	body, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	q := url.Values{"sid": {*sid}, "key": {*key}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, endpoint := listen.HookClient(*u)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"?"+q.Encode(), bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if res, err := client.Do(req); err == nil {
		res.Body.Close()
	}
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	allow, err := netallow.Parse(cfg.Allow)
	if err != nil {
		return fmt.Errorf("TETHER_ALLOW: %w", err)
	}
	claudeBin, err := exec.LookPath(cfg.ClaudeBin)
	if err != nil {
		return fmt.Errorf("claude not found (%s): %w", cfg.ClaudeBin, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	claudeDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeDir == "" {
		claudeDir = filepath.Join(home, ".claude")
	}

	ln, err := listen.Listen(cfg.Addr)
	if err != nil {
		return err
	}

	peers, err := peer.NewStore(cfg.DataDir)
	if err != nil {
		return err
	}
	peerAudit, err := peer.NewAudit(cfg.DataDir, 10000)
	if err != nil {
		return err
	}
	launcher := &session.Launcher{
		Bin:             claudeBin,
		HookExe:         exe,
		HookURL:         listen.HookTarget(ln),
		ClaudeConfigDir: claudeDir,
		Shell:           os.Getenv("SHELL"),
		// 接続先が登録されているときだけ、Claude Codeにリモート連携のツールを渡す
		RemoteTools: func() bool { return len(peers.Remotes()) > 0 },
	}
	hub := events.NewHub()
	sessions, err := session.NewManager(cfg.DataDir, launcher.Command, cfg.IdleTimeout)
	if err != nil {
		return err
	}
	notifier := &events.Notifier{Hub: hub, Sessions: sessions, Discord: cfg.DiscordWebhook, Debounce: 10 * time.Second}
	broker := peer.NewBroker(5 * time.Minute)
	broker.OnChange = func() { hub.Publish(map[string]any{"type": "remote.changed"}) }
	broker.OnRequest = func(a peer.Approval) {
		notifier.Send(events.Notification{Kind: "remote", Session: a.Session, Label: a.SessionLabel, Message: fmt.Sprintf("%sでの%sの承認を待っています", a.Machine, peer.OpLabel(a.Op))})
	}
	sessions.OnEvent = func(e session.Event) {
		hub.Publish(e)
		// 止めた・消したセッションの「まとめて許可」は残さない
		if e.Type == "session.exited" || e.Type == "session.deleted" {
			broker.DropSession(e.Session)
		}
	}

	sched, err := schedule.New(cfg.DataDir, scheduleTarget{sessions})
	if err != nil {
		return err
	}
	snips, err := snippets.New(cfg.DataDir)
	if err != nil {
		return err
	}
	sched.OnFire = func(it schedule.Item) {
		hub.Publish(map[string]any{"type": "schedule.changed", "session": it.SessionID})
		if it.Status == schedule.Failed {
			notifier.Send(events.Notification{Kind: "schedule", Session: it.SessionID, Label: "予約プロンプト", Message: "予約プロンプトの送信に失敗しました: " + it.Error})
		}
	}
	stop := make(chan struct{})
	go sched.Run(15*time.Second, stop)

	hosts := guard.NewHosts(append(guard.DetectNames(), cfg.Hosts...)...)
	static := web.Dist()
	if static == nil {
		log.Printf("web UI is not built; run `make web` (API only)")
	}
	srv := &http.Server{
		Handler: allow.Middleware(server.New(server.Deps{
			Root: cfg.Root, Token: cfg.AuthToken(), Sessions: sessions, Hub: hub, Notifier: notifier, Scheduler: sched, Snippets: snips,
			Usage:        &usage.Client{CredentialsPath: filepath.Join(claudeDir, ".credentials.json"), Endpoint: usage.DefaultEndpoint, TTL: time.Minute},
			Static:       static,
			Hosts:        hosts,
			Transcripts:  launcher.TranscriptPath,
			StartupDelay: 8 * time.Second,
			Version:      version,
			Repository:   repository,
			Peers:        peers,
			PeerAudit:    peerAudit,
			Broker:       broker,
			Shell:        os.Getenv("SHELL"),
		}).Handler()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if cfg.Auth {
		log.Printf("token authentication: on")
	} else {
		log.Printf("token authentication: off (access is limited by TETHER_ALLOW=%s; set TETHER_AUTH=on to require a token)", cfg.Allow)
		if _, isTCP := ln.Addr().(*net.TCPAddr); isTCP {
			// TCPのloopbackは同じマシンの他ユーザーやホストネットワークのコンテナからも届く
			log.Printf("WARNING: listening on TCP without authentication; other local users and host-network containers can connect. Use TETHER_ADDR=unix:<path> with tailscale serve, or TETHER_AUTH=on")
		}
		if strings.Contains(","+strings.ReplaceAll(cfg.Allow, " ", "")+",", ",all,") {
			log.Printf("WARNING: TETHER_ALLOW includes 'all' and authentication is off; anyone who can reach %s can run commands as this user", cfg.Addr)
		}
	}
	where := "http://" + ln.Addr().String()
	if _, isUnix := ln.Addr().(*net.UnixAddr); isUnix {
		where = "unix:" + ln.Addr().String()
	}
	log.Printf("tether %s listening on %s (allow %s, root %s)", version, where, cfg.Allow, cfg.Root)
	log.Printf("accepted Host names: %s (plus any IP address)", strings.Join(hosts.Names(), ", "))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Printf("shutting down")
	close(stop)
	shutdownCtx, c2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer c2()
	srv.Shutdown(shutdownCtx)
	// プロセスは止めるが記録は残すので、次回起動時にclaude --resumeで再開できる
	sessions.Shutdown()
	return nil
}

// scheduleTarget adapts the session manager to the scheduler.
type scheduleTarget struct{ m *session.Manager }

func (t scheduleTarget) Ensure(id string) (bool, error) {
	s, err := t.m.Get(id)
	if err != nil {
		return false, err
	}
	wasRunning := s.Running()
	if _, err := t.m.Ensure(id); err != nil {
		return false, err
	}
	return !wasRunning, nil
}

func (t scheduleTarget) Write(id string, p []byte) error {
	s, err := t.m.Get(id)
	if err != nil {
		return err
	}
	return s.Write(p)
}

// mcpServe runs the tether-remote MCP server over stdio for one Claude Code
// session. It forwards tools/list and tools/call to the tether server, which
// asks the user for approval before contacting another machine.
func mcpServe(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	u := fs.String("url", "", "tether endpoint (same as hook --url)")
	sid := fs.String("sid", "", "tether session id")
	key := fs.String("key", "", "session key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *u == "" || *sid == "" || *key == "" {
		return errors.New("usage: tether mcp --url <endpoint> --sid <session> --key <key>")
	}
	client, endpoint := listen.Endpoint(*u, "/internal/mcp")
	endpoint += "?" + url.Values{"sid": {*sid}, "key": {*key}}.Encode()
	return peer.ServeMCP(context.Background(), os.Stdin, os.Stdout, &mcpBackend{client: client, endpoint: endpoint}, version)
}

// mcpBackend forwards MCP calls to the tether server.
type mcpBackend struct {
	client   *http.Client
	endpoint string
}

func (b *mcpBackend) post(ctx context.Context, body, out any) error {
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 1000))
		return fmt.Errorf("tether returned %d: %s", res.StatusCode, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func (b *mcpBackend) ListTools(ctx context.Context) ([]peer.Tool, error) {
	var v struct {
		Tools []peer.Tool `json:"tools"`
	}
	err := b.post(ctx, map[string]any{"method": "tools/list"}, &v)
	return v.Tools, err
}

func (b *mcpBackend) CallTool(ctx context.Context, name string, args json.RawMessage) (peer.ToolResult, error) {
	var v struct {
		Text    string `json:"text"`
		IsError bool   `json:"isError"`
	}
	err := b.post(ctx, map[string]any{"method": "tools/call", "name": name, "arguments": args}, &v)
	return peer.ToolResult{Text: v.Text, IsError: v.IsError}, err
}
