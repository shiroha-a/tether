// Command tether is a web UI for running and sharing Claude Code sessions.
package main

import (
	"bytes"
	"context"
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
	"tether/internal/schedule"
	"tether/internal/server"
	"tether/internal/session"
	"tether/internal/usage"
	"tether/web"
)

var version = "dev"

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
	case "version", "--version", "-v":
		fmt.Println("tether", version)
	default:
		fmt.Fprintf(os.Stderr, "usage: tether [serve|hook|version]\n")
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

	launcher := &session.Launcher{
		Bin:             claudeBin,
		HookExe:         exe,
		HookURL:         listen.HookTarget(ln),
		ClaudeConfigDir: claudeDir,
		Shell:           os.Getenv("SHELL"),
	}
	hub := events.NewHub()
	sessions, err := session.NewManager(cfg.DataDir, launcher.Command, cfg.IdleTimeout)
	if err != nil {
		return err
	}
	sessions.OnEvent = func(e session.Event) { hub.Publish(e) }
	notifier := &events.Notifier{Hub: hub, Sessions: sessions, Discord: cfg.DiscordWebhook, Debounce: 10 * time.Second}

	sched, err := schedule.New(cfg.DataDir, scheduleTarget{sessions})
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
			Root: cfg.Root, Token: cfg.AuthToken(), Sessions: sessions, Hub: hub, Notifier: notifier, Scheduler: sched,
			Usage:        &usage.Client{CredentialsPath: filepath.Join(claudeDir, ".credentials.json"), Endpoint: usage.DefaultEndpoint, TTL: time.Minute},
			Static:       static,
			Hosts:        hosts,
			Transcripts:  launcher.TranscriptPath,
			StartupDelay: 8 * time.Second,
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
