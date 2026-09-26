// Package transcript turns Claude Code conversation transcripts (JSONL files
// under ~/.claude/projects) into a compact list of chat items for the UI.
//
// The transcript format is not a public API, so parsing is tolerant: unknown
// line types and block types are skipped rather than treated as errors.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Item kinds.
const (
	KindUser       = "user"
	KindAssistant  = "assistant"
	KindThinking   = "thinking"
	KindToolUse    = "tool_use"
	KindToolResult = "tool_result"
	KindCommand    = "command"
	KindNote       = "note"
)

// MaxText is the maximum length of any text field sent to the UI.
const MaxText = 4000

// Item is one chat element.
type Item struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Text      string    `json:"text,omitempty"`
	ToolID    string    `json:"toolId,omitempty"`
	ToolName  string    `json:"toolName,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	IsError   bool      `json:"isError,omitempty"`
	Truncated bool      `json:"truncated,omitempty"`
	At        time.Time `json:"at"`
}

type line struct {
	Type        string          `json:"type"`
	UUID        string          `json:"uuid"`
	Timestamp   time.Time       `json:"timestamp"`
	IsMeta      bool            `json:"isMeta"`
	IsSidechain bool            `json:"isSidechain"`
	Message     json.RawMessage `json:"message"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// ReadFrom parses complete lines of the file starting at byte offset and
// returns the items and the offset just past the last complete line. A partial
// last line (still being written) is left for the next call.
func ReadFrom(path string, offset int64) ([]Item, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && offset > info.Size() {
		// ファイルが短くなった（作り直された）場合は最初から読む
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	var items []Item
	next := offset
	for {
		raw, err := r.ReadBytes('\n')
		if len(raw) > 0 && raw[len(raw)-1] == '\n' {
			next += int64(len(raw))
			items = append(items, parseLine(raw)...)
		}
		if err != nil {
			break
		}
	}
	return items, next, nil
}

func parseLine(raw []byte) []Item {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	var l line
	if json.Unmarshal(raw, &l) != nil {
		return nil
	}
	// メタ情報（コマンドの注意書き等）とサブエージェントの内部のやり取りは会話として見せない
	if l.IsMeta || l.IsSidechain || (l.Type != "user" && l.Type != "assistant") || len(l.Message) == 0 {
		return nil
	}
	var m message
	if json.Unmarshal(l.Message, &m) != nil {
		return nil
	}
	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		if it, ok := userText(l, text); ok {
			return []Item{it}
		}
		return nil
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return nil
	}
	var out []Item
	for i, b := range blocks {
		id := l.UUID + ":" + strconv.Itoa(i)
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			if l.Type == "user" {
				if it, ok := userText(l, b.Text); ok {
					it.ID = id
					out = append(out, it)
				}
				continue
			}
			out = append(out, clip(Item{ID: id, Kind: KindAssistant, Text: b.Text, At: l.Timestamp}))
		case "thinking":
			if strings.TrimSpace(b.Thinking) == "" {
				continue
			}
			out = append(out, clip(Item{ID: id, Kind: KindThinking, Text: b.Thinking, At: l.Timestamp}))
		case "tool_use":
			out = append(out, clip(Item{
				ID: id, Kind: KindToolUse, ToolID: b.ID, ToolName: b.Name,
				Summary: summarize(b.Input), Text: prettyJSON(b.Input), At: l.Timestamp,
			}))
		case "tool_result":
			out = append(out, clip(Item{
				ID: id, Kind: KindToolResult, ToolID: b.ToolUseID, IsError: b.IsError,
				Text: resultText(b.Content), At: l.Timestamp,
			}))
		case "image":
			if l.Type == "user" {
				out = append(out, Item{ID: id, Kind: KindUser, Text: "[画像]", At: l.Timestamp})
			}
		}
	}
	return out
}

var (
	commandName   = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	commandArgs   = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	commandStdout = regexp.MustCompile(`(?s)<local-command-stdout>(.*?)</local-command-stdout>`)
)

// userText classifies a plain-text user message.
func userText(l line, text string) (Item, bool) {
	it := Item{ID: l.UUID, Kind: KindUser, At: l.Timestamp}
	switch {
	case strings.TrimSpace(text) == "":
		return it, false
	case commandName.MatchString(text):
		// スラッシュコマンド（/clear等）は発言ではなく操作として小さく出す
		it.Kind = KindCommand
		it.Text = strings.TrimSpace(commandName.FindStringSubmatch(text)[1])
		if m := commandArgs.FindStringSubmatch(text); m != nil && strings.TrimSpace(m[1]) != "" {
			it.Text += " " + strings.TrimSpace(m[1])
		}
	case commandStdout.MatchString(text):
		it.Kind = KindCommand
		it.Text = strings.TrimSpace(commandStdout.FindStringSubmatch(text)[1])
		if it.Text == "" {
			return it, false
		}
	case strings.HasPrefix(text, "<local-command-caveat>") || strings.HasPrefix(text, "<system-reminder>"):
		return it, false
	case strings.HasPrefix(text, "[Request interrupted"):
		it.Kind = KindNote
		it.Text = "中断しました"
	default:
		it.Text = text
	}
	return clip(it), true
}

// summaryKeys are tool input fields that best describe a call, in priority order.
var summaryKeys = []string{"description", "command", "file_path", "path", "pattern", "url", "query", "prompt", "skill"}

func summarize(input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range summaryKeys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			s = strings.Join(strings.Fields(s), " ")
			return truncateRunes(s, 200)
		}
	}
	return ""
}

func prettyJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// resultText flattens tool_result content, which is either a string or a list of blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[画像]")
		}
	}
	return strings.Join(parts, "\n")
}

func clip(it Item) Item {
	if t := truncateRunes(it.Text, MaxText); t != it.Text {
		it.Text, it.Truncated = t, true
	}
	return it
}

// truncateRunes cuts s to at most n runes without splitting a character.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos] + "…"
		}
		i++
	}
	return s
}
