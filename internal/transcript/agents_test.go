package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentReaderFollowsSubagentTranscripts(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "conv.jsonl")
	sub := filepath.Join(dir, "conv", "subagents")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "agent-ab12.meta.json"), []byte(`{"agentType":"Explore","description":"調査","toolUseId":"toolu_1"}`), 0o644)
	// 頼まれていないエージェントは返さない
	os.WriteFile(filepath.Join(sub, "agent-cd34.meta.json"), []byte(`{"description":"別件","toolUseId":"toolu_2"}`), 0o644)
	os.WriteFile(filepath.Join(sub, "agent-cd34.jsonl"), []byte(toolLine("y1", "Grep", `{"pattern":"x"}`)+"\n"), 0o644)
	agentFile := filepath.Join(sub, "agent-ab12.jsonl")
	os.WriteFile(agentFile, []byte(
		`{"type":"user","isSidechain":true,`+ts+`,"message":{"role":"user","content":"調べて"}}`+"\n"+
			toolLine("x1", "Grep", `{"pattern":"TODO"}`)+"\n"), 0o644)

	var r AgentReader
	got := r.Read(main, []string{"toolu_1", "toolu_other"})
	if len(got) != 1 {
		t.Fatalf("agents = %+v", got)
	}
	a := got[0]
	if a.AgentID != "ab12" || a.Description != "調査" || a.AgentType != "Explore" || a.ToolUseID != "toolu_1" ||
		a.ToolCount != 1 || a.LastTool != "Grep" || a.LastSummary != "TODO" || a.LastAt.IsZero() {
		t.Fatalf("agent = %+v", a)
	}

	// 追記された分だけ読み、数え直さない。書きかけの行は次回に回す
	f, _ := os.OpenFile(agentFile, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(toolLine("x2", "Read", `{"file_path":"/a.go"}`) + "\n" + `{"type":"assistant"`)
	f.Close()
	a = r.Read(main, []string{"toolu_1"})[0]
	if a.ToolCount != 2 || a.LastTool != "Read" || a.LastSummary != "/a.go" {
		t.Fatalf("after append = %+v", a)
	}
	if n := len(r.Read(main, nil)); n != 0 {
		t.Fatalf("no ids: %d agents", n)
	}
	a = r.Read(main, []string{"toolu_1"})[0]
	if a.ToolCount != 2 {
		t.Fatalf("re-read counted again: %+v", a)
	}
}
