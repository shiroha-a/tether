// Package transcript turns Claude Code conversation transcripts (JSONL files
// under ~/.claude/projects) into a compact list of chat items for the UI.
//
// The transcript format is not a public API, so parsing is tolerant: unknown
// line types and block types are skipped rather than treated as errors.
package transcript

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
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
	// KindSummary is the summary Claude Code writes when it compacts the conversation.
	KindSummary = "summary"
	// KindTaskEvent reports that a background shell, agent or monitor finished or emitted an event.
	KindTaskEvent = "task_event"
)

// MaxText is the maximum length of any text field sent to the UI.
const MaxText = 4000

// MaxPatchLines is the maximum number of diff lines in one item.
const MaxPatchLines = 400

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
	// Patch is the change an Edit/MultiEdit/Write call makes (from its input on
	// tool_use, from the recorded result on tool_result).
	Patch []Hunk `json:"patch,omitempty"`
	// Todos is the full list sent by TodoWrite.
	Todos []Todo `json:"todos,omitempty"`
	// Task is the task a TaskCreate/TaskUpdate call creates or changes.
	Task *Todo `json:"task,omitempty"`
	// TaskID identifies a background task: on tool_result the id Claude Code
	// assigned (shell, monitor, agent, or the TaskCreate task number), on
	// tool_use the task a call such as TaskStop refers to, on task_event the
	// task that finished.
	TaskID string `json:"taskId,omitempty"`
	// Background marks tool calls that keep running after they return.
	Background bool `json:"background,omitempty"`
	// Status is the state reported by a task_event (completed, failed, killed, ...).
	Status string `json:"status,omitempty"`
	// Images point at image blocks in the transcript (see ReadImage).
	Images []ImageRef `json:"images,omitempty"`
	// Plain marks text that is raw output (monitor events) rather than Markdown.
	Plain bool `json:"plain,omitempty"`
}

// Hunk is one block of a unified diff. Lines start with ' ', '+' or '-'.
// Start lines are 0 when unknown (diffs built from tool input).
type Hunk struct {
	OldStart int      `json:"oldStart"`
	NewStart int      `json:"newStart"`
	Lines    []string `json:"lines"`
}

// Todo is one entry of Claude Code's task list.
type Todo struct {
	ID         string `json:"id,omitempty"`
	Content    string `json:"content,omitempty"`
	ActiveForm string `json:"activeForm,omitempty"`
	Status     string `json:"status,omitempty"`
}

// ImageRef locates an image block: the byte offset of its transcript line,
// the index of the content block, and for images inside a tool_result the
// index within that result's content (otherwise -1).
type ImageRef struct {
	Line  int64 `json:"line"`
	Index int   `json:"index"`
	Sub   int   `json:"sub"`
}

type line struct {
	Type        string    `json:"type"`
	UUID        string    `json:"uuid"`
	Timestamp   time.Time `json:"timestamp"`
	IsMeta      bool      `json:"isMeta"`
	IsSidechain bool      `json:"isSidechain"`
	// IsCompactSummary marks the summary that replaces the history after /compact.
	IsCompactSummary bool                  `json:"isCompactSummary"`
	Origin           struct{ Kind string } `json:"origin"`
	Message          json.RawMessage       `json:"message"`
	// ToolUseResult is the structured result of the tool call answered on this line.
	ToolUseResult json.RawMessage `json:"toolUseResult"`
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
	Source    *imageSource    `json:"source"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
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
			items = append(items, parseLine(raw, next)...)
			next += int64(len(raw))
		}
		if err != nil {
			break
		}
	}
	return items, next, nil
}

// parseLine turns one transcript line, which starts at byte offset at, into items.
func parseLine(raw []byte, at int64) []Item {
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
	// ユーザーが貼った画像は同じ発言の文章にまとめて出す
	var userImages []ImageRef
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
			out = append(out, toolUse(Item{
				ID: id, Kind: KindToolUse, ToolID: b.ID, ToolName: b.Name,
				Summary: summarize(b.Input), Text: prettyJSON(b.Input), At: l.Timestamp,
			}, b.Input))
		case "tool_result":
			it := clip(Item{
				ID: id, Kind: KindToolResult, ToolID: b.ToolUseID, IsError: b.IsError,
				Text: resultText(b.Content), At: l.Timestamp, Images: resultImages(b.Content, at, i),
			})
			out = append(out, withToolUseResult(it, l.ToolUseResult))
		case "image":
			if l.Type == "user" && b.Source != nil {
				userImages = append(userImages, ImageRef{Line: at, Index: i, Sub: -1})
			}
		}
	}
	if len(userImages) > 0 {
		attached := false
		for i := range out {
			if out[i].Kind == KindUser {
				out[i].Images, attached = userImages, true
				break
			}
		}
		if !attached {
			out = append(out, Item{ID: l.UUID + ":images", Kind: KindUser, At: l.Timestamp, Images: userImages})
		}
	}
	return out
}

// toolUse adds the structured parts of a tool call: the diff an edit makes,
// task list changes, and whether it keeps running in the background.
func toolUse(it Item, input json.RawMessage) Item {
	var in struct {
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Content   string `json:"content"`
		Edits     []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits"`
		Todos           []Todo `json:"todos"`
		Subject         string `json:"subject"`
		ActiveForm      string `json:"activeForm"`
		TaskID          string `json:"taskId"`
		Status          string `json:"status"`
		TaskIDSnake     string `json:"task_id"`
		RunInBackground bool   `json:"run_in_background"`
	}
	if json.Unmarshal(input, &in) != nil {
		return clip(it)
	}
	switch it.ToolName {
	case "Edit":
		it.Patch = []Hunk{replaceHunk(in.OldString, in.NewString)}
	case "MultiEdit":
		for _, e := range in.Edits {
			it.Patch = append(it.Patch, replaceHunk(e.OldString, e.NewString))
		}
	case "Write":
		it.Patch = []Hunk{{Lines: prefixLines("+", in.Content)}}
	case "TodoWrite":
		it.Todos = in.Todos
	case "TaskCreate":
		it.Task = &Todo{Content: in.Subject, ActiveForm: in.ActiveForm, Status: "pending"}
	case "TaskUpdate":
		it.Task = &Todo{ID: in.TaskID, Content: in.Subject, ActiveForm: in.ActiveForm, Status: in.Status}
	case "Monitor":
		// Monitorは戻ってきた後も監視を続ける
		it.Background = true
	}
	if in.RunInBackground {
		it.Background = true
	}
	// TaskStop等が対象にするバックグラウンドのタスク（TaskUpdateのtaskIdはタスク一覧の番号なので別物）
	it.TaskID = in.TaskIDSnake
	it = clip(it)
	it.Patch, it.Truncated = clipPatch(it.Patch, it.Truncated)
	return it
}

// taskCreated matches the result of TaskCreate, which carries the new task number.
var taskCreated = regexp.MustCompile(`^Task #(\S+) created successfully`)

// withToolUseResult adds what the structured result of a tool call tells:
// the applied diff and the id of a background task.
func withToolUseResult(it Item, raw json.RawMessage) Item {
	if m := taskCreated.FindStringSubmatch(it.Text); m != nil {
		it.TaskID = m[1]
	}
	var r struct {
		Type            string          `json:"type"`
		Content         json.RawMessage `json:"content"`
		StructuredPatch []Hunk          `json:"structuredPatch"`
		BackgroundTask  string          `json:"backgroundTaskId"`
		TaskID          string          `json:"taskId"`
		AgentID         string          `json:"agentId"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &r) != nil {
		return it
	}
	it.TaskID = cmp.Or(r.BackgroundTask, r.TaskID, r.AgentID, it.TaskID)
	if len(r.StructuredPatch) > 0 {
		it.Patch = r.StructuredPatch
	} else if r.Type == "create" {
		var content string
		if json.Unmarshal(r.Content, &content) == nil {
			it.Patch = []Hunk{{OldStart: 0, NewStart: 1, Lines: prefixLines("+", content)}}
		}
	}
	it.Patch, it.Truncated = clipPatch(it.Patch, it.Truncated)
	return it
}

// replaceHunk is the diff of a string replacement whose position is unknown.
func replaceHunk(oldS, newS string) Hunk {
	return Hunk{Lines: append(prefixLines("-", oldS), prefixLines("+", newS)...)}
}

func prefixLines(prefix, s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return lines
}

// clipPatch keeps at most MaxPatchLines lines in total, each at most MaxText runes.
func clipPatch(p []Hunk, truncated bool) ([]Hunk, bool) {
	if len(p) == 0 {
		return nil, truncated
	}
	out := make([]Hunk, 0, len(p))
	left := MaxPatchLines
	for _, h := range p {
		if left == 0 {
			return out, true
		}
		if len(h.Lines) > left {
			h.Lines, truncated = h.Lines[:left], true
		}
		lines := make([]string, len(h.Lines))
		for i, l := range h.Lines {
			lines[i] = truncateRunes(l, MaxText)
		}
		h.Lines = lines
		left -= len(lines)
		out = append(out, h)
	}
	return out, truncated
}

// resultImages returns references to the images inside a tool_result block.
func resultImages(raw json.RawMessage, at int64, index int) []ImageRef {
	var blocks []block
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var refs []ImageRef
	for j, b := range blocks {
		if b.Type == "image" && b.Source != nil {
			refs = append(refs, ImageRef{Line: at, Index: index, Sub: j})
		}
	}
	return refs
}

var (
	commandName   = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	commandArgs   = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	commandStdout = regexp.MustCompile(`(?s)<local-command-stdout>(.*?)</local-command-stdout>`)
	bashInput     = regexp.MustCompile(`(?s)<bash-input>(.*?)</bash-input>`)
	bashStdout    = regexp.MustCompile(`(?s)<bash-stdout>(.*?)</bash-stdout>`)
	bashStderr    = regexp.MustCompile(`(?s)<bash-stderr>(.*?)</bash-stderr>`)
)

// ansiEscape matches terminal control sequences (colors etc.) left in command output.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// plainText decodes the entities Claude Code writes inside its XML-like tags
// (e.g. "==&gt;") and drops terminal escape sequences.
func plainText(s string) string {
	return ansiEscape.ReplaceAllString(html.UnescapeString(s), "")
}

// tag returns the trimmed contents of the first <name>...</name> in s.
func tag(s, name string) string {
	open, closing := "<"+name+">", "</"+name+">"
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, closing)
	if j < 0 {
		return ""
	}
	return plainText(strings.TrimSpace(rest[:j]))
}

// taskEvent turns a <task-notification> into a task_event item.
func taskEvent(it Item, text string) Item {
	it.Kind = KindTaskEvent
	it.TaskID = tag(text, "task-id")
	it.ToolID = tag(text, "tool-use-id")
	it.Status = tag(text, "status")
	it.Summary = truncateRunes(tag(text, "summary"), 300)
	// エージェントは最終報告（result、Markdown）、Monitorは届いたイベント（event、コマンドの出力そのまま）を本文にする
	if result := tag(text, "result"); result != "" {
		it.Text = result
	} else {
		it.Text, it.Plain = tag(text, "event"), true
	}
	return clip(it)
}

// userText classifies a plain-text user message.
func userText(l line, text string) (Item, bool) {
	it := Item{ID: l.UUID, Kind: KindUser, At: l.Timestamp}
	switch {
	case strings.TrimSpace(text) == "":
		return it, false
	case l.IsCompactSummary:
		// compactの要約はユーザーの発言ではなく、Claude Codeが差し込む前置き
		it.Kind = KindSummary
		it.Text = text
	case l.Origin.Kind == "task-notification" || (l.Origin.Kind == "" && strings.HasPrefix(strings.TrimSpace(text), "<task-notification>")):
		// バックグラウンドのシェル・エージェント・監視の完了通知も、ユーザーの発言ではない。
		// 発言の種類（origin）が記録されていればそれに従い、無い古い記録だけ本文で見分ける
		return taskEvent(it, text), true
	case bashInput.MatchString(text):
		it.Kind = KindCommand
		it.Text = "! " + plainText(strings.TrimSpace(bashInput.FindStringSubmatch(text)[1]))
	case bashStdout.MatchString(text) || bashStderr.MatchString(text):
		it.Kind = KindCommand
		var parts []string
		for _, re := range []*regexp.Regexp{bashStdout, bashStderr} {
			if m := re.FindStringSubmatch(text); m != nil && strings.TrimSpace(m[1]) != "" {
				parts = append(parts, plainText(strings.TrimSpace(m[1])))
			}
		}
		if len(parts) == 0 {
			return it, false
		}
		it.Text = strings.Join(parts, "\n")
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

// ErrNoImage is returned by ReadImage when ref does not point at a supported image.
var ErrNoImage = errors.New("transcript: no image at this position")

// imageTypes are the media types ReadImage serves.
var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// maxImageLine bounds the transcript line ReadImage reads (images are inline base64).
const maxImageLine = 64 << 20

// ReadImage decodes the image block ref points at and returns its media type and bytes.
func ReadImage(path string, ref ImageRef) (string, []byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", nil, err
	}
	if ref.Line < 0 || ref.Line >= info.Size() || ref.Index < 0 {
		return "", nil, ErrNoImage
	}
	// 行の途中を指していると、残りの部分はJSONとして読めない（後ろに余分な文字が残る）ので下で弾かれる
	r := bufio.NewReader(io.LimitReader(io.NewSectionReader(f, ref.Line, info.Size()-ref.Line), maxImageLine))
	raw, err := r.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", nil, err
	}
	var l line
	var m message
	var blocks []block
	if json.Unmarshal(raw, &l) != nil || json.Unmarshal(l.Message, &m) != nil || json.Unmarshal(m.Content, &blocks) != nil ||
		ref.Index >= len(blocks) {
		return "", nil, ErrNoImage
	}
	b := blocks[ref.Index]
	if ref.Sub >= 0 {
		var sub []block
		if b.Type != "tool_result" || json.Unmarshal(b.Content, &sub) != nil || ref.Sub >= len(sub) {
			return "", nil, ErrNoImage
		}
		b = sub[ref.Sub]
	}
	if b.Type != "image" || b.Source == nil || b.Source.Type != "base64" || !imageTypes[b.Source.MediaType] {
		return "", nil, ErrNoImage
	}
	data, err := base64.StdEncoding.DecodeString(b.Source.Data)
	if err != nil {
		return "", nil, ErrNoImage
	}
	return b.Source.MediaType, data, nil
}
