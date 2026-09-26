package session

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Launcher builds the command that runs Claude Code for a session.
type Launcher struct {
	// Bin is the claude executable.
	Bin string
	// HookExe is the tether executable used as the hook command.
	HookExe string
	// HookURL is the internal hook endpoint.
	HookURL string
	// ClaudeConfigDir is where Claude Code keeps transcripts (~/.claude).
	ClaudeConfigDir string
	// Env is the base environment. nil means os.Environ().
	Env []string
	// Shell is the login shell for shell sessions ($SHELL, falling back to /bin/bash).
	Shell string
}

// PermissionModes lists values accepted by `claude --permission-mode`.
var PermissionModes = []string{"default", "acceptEdits", "plan", "auto", "bypassPermissions"}

// Command returns the exec.Cmd for spec. When the conversation transcript
// already exists it is resumed; otherwise a new conversation is created
// with the recorded session id.
func (l *Launcher) Command(spec Spec) *exec.Cmd {
	if spec.IsShell() {
		return l.shellCommand(spec)
	}
	args := []string{}
	if l.transcriptExists(spec.ClaudeSessionID) {
		args = append(args, "--resume", spec.ClaudeSessionID)
	} else {
		args = append(args, "--session-id", spec.ClaudeSessionID)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.PermissionMode != "" && spec.PermissionMode != "default" {
		args = append(args, "--permission-mode", spec.PermissionMode)
	}
	args = append(args, "--settings", l.settingsJSON(spec))
	cmd := exec.Command(l.Bin, args...)
	cmd.Dir = spec.Cwd
	cmd.Env = l.env()
	return cmd
}

// shellCommand starts the user's login shell. It gets a real pty, so
// programs that need a terminal (sudo password prompts, editors) work.
func (l *Launcher) shellCommand(spec Spec) *exec.Cmd {
	shell := l.Shell
	if shell == "" {
		shell = "/bin/bash"
	}
	cmd := exec.Command(shell, "-l")
	cmd.Dir = spec.Cwd
	cmd.Env = l.env()
	return cmd
}

// transcriptExists reports whether Claude Code has a saved conversation for id.
// Claude Code stores transcripts at <config>/projects/<encoded cwd>/<id>.jsonl;
// globbing avoids depending on the cwd encoding rule.
func (l *Launcher) transcriptExists(id string) bool {
	_, ok := l.TranscriptPath(id)
	return ok
}

// TranscriptPath returns the transcript file of a Claude Code conversation.
func (l *Launcher) TranscriptPath(id string) (string, bool) {
	// idはパスの一部になるので、グロブやパス区切りを含むものは受け付けない
	if id == "" || l.ClaudeConfigDir == "" || strings.ContainsAny(id, `/\*?[`) {
		return "", false
	}
	matches, _ := filepath.Glob(filepath.Join(l.ClaudeConfigDir, "projects", "*", id+".jsonl"))
	if len(matches) == 0 {
		return "", false
	}
	return matches[0], true
}

type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type hookMatcher struct {
	Hooks []hookCommand `json:"hooks"`
}

func (l *Launcher) settingsJSON(spec Spec) string {
	cmd := strings.Join([]string{
		shellQuote(l.HookExe), "hook",
		"--url", shellQuote(l.HookURL),
		"--sid", shellQuote(spec.ID),
		"--key", shellQuote(spec.HookKey),
	}, " ")
	m := []hookMatcher{{Hooks: []hookCommand{{Type: "command", Command: cmd}}}}
	settings := map[string]any{
		"hooks": map[string]any{
			"Stop":             m,
			"Notification":     m,
			"SessionStart":     m,
			"UserPromptSubmit": m,
			// 許可確認の前に届くので、遅れて届く選択待ちの通知が回答済みかを判断するのに使う
			"PreToolUse": m,
		},
	}
	b, _ := json.Marshal(settings)
	return string(b)
}

// serverOnlyEnvPrefixes are environment variables that must not leak into sessions.
// The CLAUDE* entries are per-process markers set when tether itself runs inside
// Claude Code; inheriting CLAUDE_CODE_CHILD_SESSION disables transcript saving,
// which would make --resume impossible.
var serverOnlyEnvPrefixes = []string{
	"TETHER_", "INVOCATION_ID=", "JOURNAL_STREAM=",
	"CLAUDECODE=", "CLAUDE_PID=", "CLAUDE_CODE_ENTRYPOINT=", "CLAUDE_CODE_CHILD_SESSION=",
	"CLAUDE_CODE_SESSION_ID=", "CLAUDE_CODE_SESSION_ATTENDED=", "CLAUDE_CODE_EXECPATH=",
	"CLAUDE_CODE_MESSAGING_SOCKET=", "CLAUDE_CODE_MESSAGING_TOKEN=",
}

func (l *Launcher) env() []string {
	base := l.Env
	if base == nil {
		base = os.Environ()
	}
	out := make([]string, 0, len(base)+2)
	for _, kv := range base {
		skip := strings.HasPrefix(kv, "TERM=") || strings.HasPrefix(kv, "COLORTERM=")
		for _, p := range serverOnlyEnvPrefixes {
			if strings.HasPrefix(kv, p) {
				skip = true
			}
		}
		if !skip {
			out = append(out, kv)
		}
	}
	return append(out, "TERM=xterm-256color", "COLORTERM=truecolor")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
