# tether

A self-hosted web app for running Claude Code from any browser — desktop, phone, or installed as a PWA. Start sessions in any folder, keep them running on your server, and pick them up from another device. The frontend is embedded in a single Go binary, so Node.js is not needed at runtime.

## Features

- **Sessions**: launch Claude Code in a folder (model, permission mode, and name are optional), or open a plain login shell. Shells run in a real pty, so `sudo` password prompts and full-screen programs work.
- **Shared across devices**: several browsers can attach to the same session at once. The pty uses the smallest attached viewport.
- **Resume**: after a stop or a server restart, Claude Code conversations resume with `claude --resume`. The session id is tracked across `/clear`.
- **Chat view**: read a Claude Code session as a web chat, built from its transcript. Tool calls appear as collapsible cards, and consecutive calls are grouped. You can send prompts, and answer permission prompts with ↑/↓/number/Enter/Esc buttons.
- **Home screen**: session cards with live status (working, waiting for a choice, done), quick launch from recent folders, recent notifications, usage, and host health (load, memory, disk, uptime).
- **Usage**: 5-hour and weekly Claude usage with reset times.
- **Files**: browse, upload, download, and create folders. Markdown, text, and images can be previewed.
- **Notifications**: browser notifications, in-app toasts, and an optional Discord webhook when Claude finishes or needs a decision.
- **Scheduled prompts**: send a prompt at a given time. A stopped session is resumed first.
- **Mobile friendly**:
  - Helper key bar with ↑/↓/Enter always visible.
  - Composer for typing with the phone keyboard.
  - Copy sheet that shows the terminal as selectable text.
  - Paste button, which needs HTTPS for direct clipboard access.
  - OSC 52 support, so apps can copy to your clipboard.
- **Themes**: automatic, light, or dark. The terminal area always stays dark because Claude Code's TUI colors assume a dark background.

## Requirements

- Linux
- Claude Code installed as `claude` on `PATH` and logged in
- Build only: Go 1.27.1 (pinned in `go.mod`; an older `go` downloads it automatically with `GOTOOLCHAIN=auto`) and Node.js 22 or later

## Build and run

```bash
make build            # builds web/dist and produces bin/tether
./bin/tether serve
# with token authentication
TETHER_AUTH=on TETHER_TOKEN=$(openssl rand -hex 24) ./bin/tether serve
```

Open http://127.0.0.1:3100, or http://&lt;tailscale-hostname-or-ip&gt;:3100 from a device on your tailnet. With `TETHER_AUTH=on`, log in with the token. By default, connections from anywhere other than localhost and Tailscale (for example your LAN or the internet) are rejected with 403.

### Development

```bash
make dev-server   # Go server on :3100
make dev-web      # Vite on :5173, proxies /api and /ws to :3100
make test         # go vet, go test -race, tsc, vitest
make fmt          # gofmt, prettier
```

## Configuration (environment variables)

| Variable | Default | Description |
|---|---|---|
| `TETHER_ADDR` | `:3100` | Listen address. A TCP address (all interfaces by default; who may connect is controlled by `TETHER_ALLOW`), or `unix:<absolute path>` for a Unix socket that only your user can open (mode 600). Use the Unix socket when authentication is off; see Security. |
| `TETHER_ALLOW` | `loopback,tailscale` | Allowed peers, comma-separated: `loopback`, `tailscale`, CIDRs such as `192.168.1.0/24`, or `all`. Unknown entries are a startup error. |
| `TETHER_HOSTS` | (none) | Extra names accepted in the `Host` header, for example a reverse-proxy domain. IP addresses, `localhost`, the machine's hostname, and its Tailscale DNS name are always accepted. |
| `TETHER_AUTH` | `off` | Token authentication (`on`/`off`). With `on`, `TETHER_TOKEN` is required and tether refuses to start without it. |
| `TETHER_TOKEN` | (none) | The access token. Only used when `TETHER_AUTH=on`; ignored otherwise. |
| `TETHER_ROOT` | `$HOME` | Root for the file browser and for launch folders. Paths outside it are rejected. |
| `TETHER_DATA_DIR` | `~/.local/share/tether` | Where sessions and scheduled prompts are stored. |
| `TETHER_CLAUDE_BIN` | `claude` | The Claude Code executable. |
| `TETHER_DISCORD_WEBHOOK` | (none) | Discord webhook URL for notifications. |
| `TETHER_IDLE_TIMEOUT` | `24h` | Stop a session's process after no browser has been attached for this long (`0` disables). Conversations can still be resumed. |

`CLAUDE_CONFIG_DIR` is honored when locating Claude Code's credentials and transcripts (default `~/.claude`).

## Run as a systemd service

```bash
make build
deploy/install-systemd.sh --dry-run   # shows what it will do and the unit file it will write
deploy/install-systemd.sh             # installs and starts the service (asks for your sudo password)
```

- Put settings in `~/.config/tether/env`; do not edit the unit file. Apply changes with `sudo systemctl restart tether`.
- After rebuilding the binary, run `sudo systemctl restart tether` to use the new version.
- If a manually started tether is running, the script asks before stopping it. Running terminals end; Claude Code conversations can be resumed.
- The script writes your Tailscale DNS name into `TETHER_HOSTS`. At boot, tether may start before Tailscale is up, and without the name, HTTPS requests through `tailscale serve` would be rejected (421).
- The unit starts tether through a login shell, so Claude Code sees the same `PATH` (`go`, `~/.local/bin`, …) as an interactive terminal.
- Run the script from SSH or a local console, not from a tether terminal, because the script stops tether.
- Other commands:
  - Check status: `deploy/install-systemd.sh status`
  - Follow logs: `journalctl -u tether -f`
  - Remove the service: `deploy/install-systemd.sh uninstall`

## HTTPS with Tailscale Serve (recommended)

Serving over HTTPS lets the phone's Paste button read the clipboard directly, because browsers only expose the clipboard API in secure contexts. tether listens on a private Unix socket, and `tailscale serve` terminates HTTPS for your tailnet and forwards to it:

```bash
# ~/.config/tether/env
TETHER_ADDR=unix:/home/<you>/.local/share/tether/tether.sock

sudo systemctl restart tether
# expose port 3100 on the tailnet over HTTPS (enable HTTPS certificates in the Tailscale admin console first)
tailscale serve --bg --https=3100 unix:/home/<you>/.local/share/tether/tether.sock
tailscale serve status
```

- **Why a Unix socket, not `127.0.0.1`**: a TCP port on localhost is reachable by every local user and by containers that share the host network. The socket is created with mode 600, so only your user (and `tailscaled`, which runs as root) can connect.

- **URL**: `https://<machine>.<tailnet>.ts.net:3100`. Using a dedicated port keeps port 443 free for other services on the same machine; add them the same way, for example with `--https=8443`.
- **Security checks still work**: requests through `tailscale serve` reach tether from localhost, and the `Host` header is preserved. The Host check, CSRF protection, and WebSocket origin checks work unchanged.

## How it works

```
Browser (React + xterm.js)  --HTTP/WebSocket-->  tether (Go)
                                                   |- pty -- claude --session-id|--resume <id> --settings <hooks>
                                                   |- pty -- $SHELL -l                     (terminal sessions)
                                                   `- /internal/hook <-- `tether hook` (called by Claude Code hooks)
```

- **Hooks**: Claude Code is started with `Stop`, `Notification`, `SessionStart`, and `UserPromptSubmit` hooks injected through `--settings`. Each hook runs the `tether hook` subcommand, which reports to the server with a random per-session key. Your `~/.claude/settings.json` is never modified.
- **Session status**: hook events drive the status shown in the UI. A prompt means working, a permission prompt or dialog means waiting for a choice, and a finished response means done.
- **Chat view**: the server reads Claude Code's transcript files (`~/.claude/projects/*/<session-id>.jsonl`) incrementally.
- **Usage**: fetched from the same OAuth usage endpoint that Claude Code uses.
- **Stopping**: stopping the server stops the Claude Code processes but keeps the session records. Opening a session again resumes it with `--resume`.

> **Note:** The transcript format and the usage endpoint are not public APIs. The chat view and the usage display may break if Claude Code changes them.

## Security

- **Access means full control**: anyone who can reach tether can run commands as your user.
- **Default protection**: token authentication is off by default. tether relies on the peer restriction (localhost and Tailscale only), `Host` header validation, and CSRF protection.
- **When to enable the token**: set `TETHER_AUTH=on` if any of these apply:
  - You share your tailnet with others.
  - Other users have accounts on this machine.
  - You widened `TETHER_ALLOW`. With `all` and authentication off, tether logs a warning at startup.
- **What counts as Tailscale**: a peer counts as Tailscale only when both its address and the local address it connected to are in Tailscale ranges (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`). Carrier-grade NAT addresses arriving on your LAN are not trusted.
- **Proxies**: `X-Forwarded-For` is ignored. Connections from a local reverse proxy such as `tailscale serve` are treated as loopback.
- **DNS rebinding protection**: the `Host` header must be an allowed name, otherwise the request gets 421. Add custom proxy domains to `TETHER_HOSTS`.
- **CSRF protection**: `POST`/`PATCH`/`DELETE` requests under `/api/` require an `X-Tether: 1` header and a same-origin `Origin`. When calling the API with curl, add `-H 'X-Tether: 1'`.
- **WebSockets**: only same-origin WebSocket connections are accepted. A cross-origin attempt is rejected before a stopped session would be resumed.
- **Chat input**: the chat input endpoint accepts prompt text and a fixed set of keys only (arrows, Enter, Esc, Tab, Shift+Tab, 1–9), never arbitrary escape sequences.
- **Downloads**: HTML and SVG files are always served as attachments, never inline, so they cannot run scripts in tether's origin.
- **Other local users and containers**: with authentication off, listen on a Unix socket (`TETHER_ADDR=unix:...`). A TCP listener, even on `127.0.0.1`, is reachable by other accounts on the machine (for example database or model servers) and by containers using the host network, which could then run commands as your user. tether logs a warning when it listens on TCP without authentication.
- **Content Security Policy**: every response carries a CSP that only allows resources from tether itself (`img-src 'self' data: blob:`, no remote scripts, no framing, no form posts). This stops rendered Markdown from loading remote images whose URLs could carry data out, for example after a prompt injection makes Claude write such an image. Remote images in chat and previews are shown as links instead and open only when tapped.
- **Clipboard writes from programs (OSC 52)**: a program's request to set the clipboard is never applied automatically. tether shows the requested text and copies it only when you tap Copy, so output from an untrusted file or command cannot silently replace what you paste later.

## License

[MIT](LICENSE) © 2026 shiroha-a
