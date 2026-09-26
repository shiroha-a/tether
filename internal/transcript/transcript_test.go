package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

const ts = `"timestamp":"2026-09-26T03:00:00.000Z"`

// assistantLine has thinking (one of them empty), text and a tool call in one message.
const assistantLine = `{"type":"assistant","uuid":"a1",` + ts + `,"message":{"role":"assistant","content":[` +
	`{"type":"thinking","thinking":"考え中","signature":"x"},` +
	`{"type":"thinking","thinking":"","signature":"redacted"},` +
	`{"type":"text","text":"了解です。**確認**します"},` +
	`{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls   -la\n/tmp","description":"一覧を表示"}}]}}`

// fixture mirrors the shapes of real Claude Code transcript lines.
var fixture = []string{
	`{"type":"mode","mode":"default"}`,
	`{"type":"user","uuid":"u1",` + ts + `,"message":{"role":"user","content":"READMEを直して"}}`,
	`{"type":"user","uuid":"meta",` + ts + `,"isMeta":true,"message":{"role":"user","content":"Base directory for this skill: /x"}}`,
	`{"type":"user","uuid":"caveat",` + ts + `,"message":{"role":"user","content":"<local-command-caveat>Caveat: ...</local-command-caveat>"}}`,
	assistantLine,
	`{"type":"user","uuid":"u2",` + ts + `,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"a.txt\nb.txt","is_error":false}]}}`,
	`{"type":"assistant","uuid":"a2",` + ts + `,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"Read","input":{"file_path":"/x/README.md"}}]}}`,
	`{"type":"user","uuid":"u3",` + ts + `,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"line1"},{"type":"image","source":{}}],"is_error":true}]}}`,
	`{"type":"assistant","uuid":"side",` + ts + `,"isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"サブエージェント内部"}]}}`,
	`{"type":"user","uuid":"cmd",` + ts + `,"message":{"role":"user","content":"<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args>opus</command-args>"}}`,
	`{"type":"user","uuid":"out",` + ts + `,"message":{"role":"user","content":"<local-command-stdout>Set model to opus</local-command-stdout>"}}`,
	`{"type":"user","uuid":"int",` + ts + `,"message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`,
	`{"type":"user","uuid":"img",` + ts + `,"message":{"role":"user","content":[{"type":"image","source":{}},{"type":"text","text":"この画像を見て"}]}}`,
	`{"type":"system","uuid":"s1",` + ts + `,"subtype":"compact_boundary"}`,
	`not json at all`,
	`{"type":"assistant","uuid":"a3",` + ts + `,"message":{"role":"assistant","content":[{"type":"server_tool_use","id":"x"},{"type":"text","text":"完了"}]}}`,
}

func writeFile(t *testing.T, lines []string, trailingNewline bool) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	body := strings.Join(lines, "\n")
	if trailingNewline {
		body += "\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadFromNormalizesMessages(t *testing.T) {
	p := writeFile(t, fixture, true)
	items, next, err := ReadFrom(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(p)
	if next != info.Size() {
		t.Fatalf("next = %d, want file size %d", next, info.Size())
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Kind+":"+it.Text)
	}
	want := []string{
		"user:READMEを直して",
		"thinking:考え中",
		"assistant:了解です。**確認**します",
		"tool_use:" + items[3].Text,
		"tool_result:a.txt\nb.txt",
		"tool_use:" + items[5].Text,
		"tool_result:line1\n[画像]",
		"command:/model opus",
		"command:Set model to opus",
		"note:中断しました",
		"user:[画像]",
		"user:この画像を見て",
		"assistant:完了",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("items:\n got  %q\n want %q", got, want)
	}

	bash := items[3]
	if bash.ToolID != "toolu_1" || bash.ToolName != "Bash" || bash.Summary != "一覧を表示" || !strings.Contains(bash.Text, `"command"`) {
		t.Fatalf("bash tool_use = %+v", bash)
	}
	if res := items[4]; res.ToolID != "toolu_1" || res.IsError {
		t.Fatalf("bash result = %+v", res)
	}
	if read := items[5]; read.Summary != "/x/README.md" {
		t.Fatalf("read summary = %q", read.Summary)
	}
	if res := items[6]; res.ToolID != "toolu_2" || !res.IsError {
		t.Fatalf("read result = %+v", res)
	}
	// 同じ行の複数ブロックはIDが重複しない
	seen := map[string]bool{}
	for _, it := range items {
		if seen[it.ID] {
			t.Fatalf("duplicate id %q", it.ID)
		}
		seen[it.ID] = true
	}
	if items[0].At.IsZero() {
		t.Fatal("timestamp not parsed")
	}
}

func TestReadFromIsIncrementalAndSkipsPartialLine(t *testing.T) {
	// 書き込み途中（改行で終わっていない）最終行は次回に回す
	p := writeFile(t, fixture[:2], false)
	items, next, err := ReadFrom(p, 0)
	if err != nil || len(items) != 0 {
		t.Fatalf("first read: %d items, err %v", len(items), err)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("\n" + assistantLine + "\n")
	f.Close()
	items, next2, err := ReadFrom(p, next)
	if err != nil || len(items) != 4 || items[0].Text != "READMEを直して" {
		t.Fatalf("second read: %+v err %v", items, err)
	}
	// 追加がなければ何も返さず、offsetも進まない
	items, next3, _ := ReadFrom(p, next2)
	if len(items) != 0 || next3 != next2 {
		t.Fatalf("third read: %d items, offset %d -> %d", len(items), next2, next3)
	}
}

func TestReadFromRestartsWhenFileShrinks(t *testing.T) {
	p := writeFile(t, fixture[1:2], true)
	items, _, err := ReadFrom(p, 1<<20)
	if err != nil || len(items) != 1 {
		t.Fatalf("offset beyond size: %d items, err %v", len(items), err)
	}
}

func TestLongTextIsTruncatedOnRuneBoundary(t *testing.T) {
	long := strings.Repeat("あ", MaxText+10)
	p := writeFile(t, []string{`{"type":"user","uuid":"u",` + ts + `,"message":{"role":"user","content":"` + long + `"}}`}, true)
	items, _, _ := ReadFrom(p, 0)
	if len(items) != 1 || !items[0].Truncated || !utf8.ValidString(items[0].Text) {
		t.Fatalf("truncated item = truncated:%v valid:%v", items[0].Truncated, utf8.ValidString(items[0].Text))
	}
	if n := utf8.RuneCountInString(items[0].Text); n != MaxText+1 {
		t.Fatalf("rune count = %d, want %d (text + ellipsis)", n, MaxText+1)
	}
}

func TestSummaryPrefersDescriptionAndCollapsesWhitespace(t *testing.T) {
	cases := map[string]string{
		`{"command":"git   status\n--short"}`:         "git status --short",
		`{"pattern":"TODO","path":"/src"}`:            "/src",
		`{"description":"テスト","command":"make test"}`: "テスト",
		`{"other":1}`:     "",
		`"not an object"`: "",
		`{"prompt":"` + strings.Repeat("x", 300) + `"}`: strings.Repeat("x", 200) + "…",
	}
	for in, want := range cases {
		if got := summarize([]byte(in)); got != want {
			t.Errorf("summarize(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestMissingFile(t *testing.T) {
	if _, _, err := ReadFrom(filepath.Join(t.TempDir(), "none.jsonl"), 0); err == nil {
		t.Fatal("expected error for missing file")
	}
}
