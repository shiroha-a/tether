package transcript

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	// 貼った画像は同じ発言の文章にまとめ、ツール結果の画像も参照を付ける
	if img := items[10]; len(img.Images) != 1 || img.Images[0].Index != 0 || img.Images[0].Sub != -1 {
		t.Fatalf("user images = %+v", img.Images)
	}
	if res := items[6]; len(res.Images) != 1 || res.Images[0].Index != 0 || res.Images[0].Sub != 1 {
		t.Fatalf("tool_result images = %+v", res.Images)
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

func readLines(t *testing.T, lines ...string) []Item {
	t.Helper()
	items, _, err := ReadFrom(writeFile(t, lines, true), 0)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestNonHumanUserMessagesAreNotShownAsUserText(t *testing.T) {
	notification := "<task-notification>\n<task-id>a1b2</task-id>\n<tool-use-id>toolu_9</tool-use-id>\n<status>completed</status>\n" +
		"<summary>Agent \"レビュー\" finished</summary>\n<note>...</note>\n<result>指摘は2件です</result>\n</task-notification>"
	monitor := "<task-notification>\n<task-id>m1</task-id>\n<summary>Monitor event: \"CI\"</summary>\n<event>\x1b[32mserver: pass &gt; ok\x1b[0m</event>\n</task-notification>\nIf this event..."
	items := readLines(t,
		`{"type":"user","uuid":"n1",`+ts+`,"origin":{"kind":"task-notification"},"message":{"role":"user","content":`+quote(notification)+`}}`,
		// originが無い古い記録でも本文で見分ける
		`{"type":"user","uuid":"n2",`+ts+`,"message":{"role":"user","content":[{"type":"text","text":`+quote(monitor)+`}]}}`,
		`{"type":"user","uuid":"c1",`+ts+`,"isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":"This session is being continued..."}}`,
		`{"type":"user","uuid":"b1",`+ts+`,"message":{"role":"user","content":"<bash-input>git status</bash-input>"}}`,
		`{"type":"user","uuid":"b2",`+ts+`,"message":{"role":"user","content":"<bash-stdout>clean \u001b[32m==&gt;\u001b[0m ok</bash-stdout><bash-stderr>a &amp;&amp; b</bash-stderr>"}}`,
		`{"type":"user","uuid":"b3",`+ts+`,"message":{"role":"user","content":"<bash-stdout></bash-stdout><bash-stderr></bash-stderr>"}}`,
		`{"type":"user","uuid":"h1",`+ts+`,"origin":{"kind":"human"},"message":{"role":"user","content":"<pasted_content>本文</pasted_content>"}}`,
		// ユーザーが通知の形の文字列を打っても、ユーザーの発言のまま
		`{"type":"user","uuid":"h2",`+ts+`,"origin":{"kind":"human"},"message":{"role":"user","content":"<task-notification>x</task-notification>"}}`,
		// originがあれば本文の形に関係なく通知として扱う
		`{"type":"user","uuid":"n3",`+ts+`,"origin":{"kind":"task-notification"},"message":{"role":"user","content":"Background task finished"}}`,
	)
	var got []string
	for _, it := range items {
		got = append(got, it.Kind+":"+it.Text)
	}
	want := []string{
		"task_event:指摘は2件です",
		"task_event:server: pass > ok",
		"summary:This session is being continued...",
		"command:! git status",
		"command:clean ==> ok\na && b",
		"user:<pasted_content>本文</pasted_content>",
		"user:<task-notification>x</task-notification>",
		"task_event:",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("items:\n got  %q\n want %q", got, want)
	}
	if ev := items[0]; ev.TaskID != "a1b2" || ev.ToolID != "toolu_9" || ev.Status != "completed" || ev.Summary != `Agent "レビュー" finished` {
		t.Fatalf("agent event = %+v", ev)
	}
	if items[0].Plain {
		t.Fatal("agent result should be Markdown")
	}
	if ev := items[1]; ev.TaskID != "m1" || ev.ToolID != "" || ev.Status != "" || !ev.Plain {
		t.Fatalf("monitor event = %+v", ev)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func toolLine(uuid, name, input string) string {
	return `{"type":"assistant","uuid":"` + uuid + `",` + ts + `,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_` + uuid +
		`","name":"` + name + `","input":` + input + `}]}}`
}

func resultLine(uuid, text, toolUseResult string) string {
	return `{"type":"user","uuid":"r` + uuid + `",` + ts + `,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_` + uuid +
		`","content":` + quote(text) + `}]},"toolUseResult":` + toolUseResult + `}`
}

func TestEditsCarryAPatch(t *testing.T) {
	items := readLines(t,
		toolLine("e1", "Edit", `{"file_path":"/a.go","old_string":"a\nb","new_string":"c\n"}`),
		resultLine("e1", "ok", `{"filePath":"/a.go","structuredPatch":[{"oldStart":3,"oldLines":2,"newStart":3,"newLines":1,"lines":[" x","-a","-b","+c"]}]}`),
		toolLine("m1", "MultiEdit", `{"file_path":"/a.go","edits":[{"old_string":"p","new_string":"q"},{"old_string":"r","new_string":""}]}`),
		toolLine("w1", "Write", `{"file_path":"/n.txt","content":"one\ntwo\n"}`),
		resultLine("w1", "created", `{"type":"create","filePath":"/n.txt","content":"one\ntwo\n","structuredPatch":[]}`),
		// 文字列の結果（エラー等）は差分なし
		resultLine("x1", "Error: Exit code 1", `"Error: Exit code 1"`),
	)
	check := func(i int, want []Hunk) {
		t.Helper()
		if !reflect.DeepEqual(items[i].Patch, want) {
			t.Fatalf("items[%d].Patch = %+v, want %+v", i, items[i].Patch, want)
		}
	}
	check(0, []Hunk{{Lines: []string{"-a", "-b", "+c"}}})
	check(1, []Hunk{{OldStart: 3, NewStart: 3, Lines: []string{" x", "-a", "-b", "+c"}}})
	check(2, []Hunk{{Lines: []string{"-p", "+q"}}, {Lines: []string{"-r"}}})
	check(3, []Hunk{{Lines: []string{"+one", "+two"}}})
	check(4, []Hunk{{NewStart: 1, Lines: []string{"+one", "+two"}}})
	check(5, nil)
}

func TestLongPatchIsClipped(t *testing.T) {
	content := strings.Repeat(`x\n`, MaxPatchLines+5)
	items := readLines(t, toolLine("w", "Write", `{"file_path":"/n","content":"`+content+`"}`))
	if n := len(items[0].Patch[0].Lines); n != MaxPatchLines || !items[0].Truncated {
		t.Fatalf("lines = %d, truncated = %v", n, items[0].Truncated)
	}
	hunks, truncated := clipPatch([]Hunk{{Lines: make([]string, MaxPatchLines)}, {Lines: []string{"+y"}}}, false)
	if len(hunks) != 1 || !truncated {
		t.Fatalf("second hunk kept after the limit: %d hunks, truncated %v", len(hunks), truncated)
	}
}

func TestTasksAndBackgroundCalls(t *testing.T) {
	items := readLines(t,
		toolLine("t1", "TaskCreate", `{"subject":"テストを書く","description":"...","activeForm":"テストを書いています"}`),
		resultLine("t1", "Task #3 created successfully: テストを書く", `{"task":{"id":"3"}}`),
		toolLine("t2", "TaskUpdate", `{"taskId":"3","status":"in_progress"}`),
		toolLine("t3", "TodoWrite", `{"todos":[{"content":"A","status":"completed","activeForm":"Aを実行中"},{"content":"B","status":"pending","activeForm":"B"}]}`),
		toolLine("b1", "Bash", `{"command":"make test","run_in_background":true}`),
		resultLine("b1", "Command running in background", `{"stdout":"","backgroundTaskId":"bx1"}`),
		toolLine("a1", "Agent", `{"description":"調査","prompt":"...","run_in_background":true}`),
		resultLine("a1", "Async agent launched", `{"isAsync":true,"status":"async_launched","agentId":"ag7"}`),
		toolLine("mo", "Monitor", `{"description":"CI","command":"gh run watch"}`),
		resultLine("mo", "started", `{"taskId":"mon1","timeoutMs":1000}`),
		toolLine("s1", "TaskStop", `{"task_id":"bx1"}`),
		toolLine("f1", "Bash", `{"command":"ls"}`),
	)
	if tk := items[0].Task; tk == nil || *tk != (Todo{Content: "テストを書く", ActiveForm: "テストを書いています", Status: "pending"}) {
		t.Fatalf("TaskCreate task = %+v", tk)
	}
	if items[1].TaskID != "3" {
		t.Fatalf("TaskCreate result id = %q", items[1].TaskID)
	}
	if tk := items[2].Task; tk == nil || *tk != (Todo{ID: "3", Status: "in_progress"}) || items[2].TaskID != "" {
		t.Fatalf("TaskUpdate = %+v taskId %q", tk, items[2].TaskID)
	}
	if todos := items[3].Todos; len(todos) != 2 || todos[0] != (Todo{Content: "A", Status: "completed", ActiveForm: "Aを実行中"}) {
		t.Fatalf("TodoWrite todos = %+v", todos)
	}
	for i, want := range map[int]string{5: "bx1", 7: "ag7", 9: "mon1"} {
		if items[i].TaskID != want {
			t.Errorf("items[%d].TaskID = %q, want %q", i, items[i].TaskID, want)
		}
	}
	for i, want := range map[int]bool{4: true, 6: true, 8: true, 11: false} {
		if items[i].Background != want {
			t.Errorf("items[%d].Background = %v, want %v", i, items[i].Background, want)
		}
	}
	if items[10].TaskID != "bx1" {
		t.Fatalf("TaskStop task id = %q", items[10].TaskID)
	}
}

func TestReadImage(t *testing.T) {
	png := []byte("\x89PNG fake")
	data := base64.StdEncoding.EncodeToString(png)
	img := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + data + `"}}`
	svg := `{"type":"image","source":{"type":"base64","media_type":"image/svg+xml","data":"` + data + `"}}`
	lines := []string{
		`{"type":"user","uuid":"u1",` + ts + `,"message":{"role":"user","content":[` + img + `,{"type":"text","text":"見て"},` + svg + `]}}`,
		`{"type":"user","uuid":"u2",` + ts + `,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"x"},` + img + `]}]}}`,
		// tool_result以外のブロックの中身は画像として読まない
		`{"type":"user","uuid":"u3",` + ts + `,"message":{"role":"user","content":[{"type":"document","content":[` + img + `]}]}}`,
	}
	p := writeFile(t, lines, true)
	items, _, _ := ReadFrom(p, 0)
	// SVGは参照だけ付くが、配信はしない
	if len(items) < 2 || len(items[0].Images) != 2 || len(items[1].Images) != 1 {
		t.Fatalf("items = %+v", items)
	}
	for _, ref := range []ImageRef{items[0].Images[0], items[1].Images[0]} {
		typ, got, err := ReadImage(p, ref)
		if err != nil || typ != "image/png" || string(got) != string(png) {
			t.Fatalf("ReadImage(%+v) = %q %q %v", ref, typ, got, err)
		}
	}
	second := items[1].Images[0].Line
	third := second + int64(len(lines[1])) + 1
	bad := []ImageRef{
		{Line: third, Index: 0, Sub: 0},   // tool_result以外のブロックの中
		items[0].Images[1],                // 許可していない形式
		{Line: 0, Index: 1, Sub: -1},      // 画像ではないブロック
		{Line: 0, Index: 9, Sub: -1},      // 範囲外のブロック
		{Line: second, Index: 0, Sub: 0},  // 結果の中の文章
		{Line: second, Index: 0, Sub: -1}, // tool_resultそのもの
		{Line: 5, Index: 0, Sub: -1},      // 行の途中
		{Line: -1, Index: 0, Sub: -1},
		{Line: 1 << 30, Index: 0, Sub: -1},
	}
	for _, ref := range bad {
		if _, _, err := ReadImage(p, ref); !errors.Is(err, ErrNoImage) {
			t.Errorf("ReadImage(%+v) err = %v, want ErrNoImage", ref, err)
		}
	}
}
