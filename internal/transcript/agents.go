package transcript

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Agent is the progress of one subagent, read from its own transcript.
type Agent struct {
	AgentID     string    `json:"agentId"`
	Description string    `json:"description,omitempty"`
	AgentType   string    `json:"agentType,omitempty"`
	ToolUseID   string    `json:"toolUseId,omitempty"`
	ToolCount   int       `json:"toolCount"`
	LastTool    string    `json:"lastTool,omitempty"`
	LastSummary string    `json:"lastSummary,omitempty"`
	LastAt      time.Time `json:"lastAt"`
}

// AgentReader reads subagent transcripts incrementally. Subagent files grow
// while the agent works, so each call only parses what was appended.
type AgentReader struct {
	mu    sync.Mutex
	cache map[string]*agentState
}

type agentState struct {
	offset int64
	agent  Agent
}

// maxAgentCache bounds the remembered files; the cache is simply reset when full.
const maxAgentCache = 256

// Read returns the progress of the subagents started by the given tool calls
// in the conversation whose transcript is at path. Calls without a subagent
// transcript (yet) are skipped.
func (r *AgentReader) Read(path string, toolUseIDs []string) []Agent {
	out := []Agent{}
	want := map[string]bool{}
	for _, id := range toolUseIDs {
		if id != "" {
			want[id] = true
		}
	}
	if len(want) == 0 {
		return out
	}
	dir := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
	metas, _ := filepath.Glob(filepath.Join(dir, "agent-*.meta.json"))
	for _, m := range metas {
		base := strings.TrimSuffix(m, ".meta.json")
		if a, ok := r.read(base, strings.TrimPrefix(filepath.Base(base), "agent-")); ok && want[a.ToolUseID] {
			out = append(out, a)
		}
	}
	return out
}

func (r *AgentReader) read(base, id string) (Agent, bool) {
	f, err := os.Open(base + ".jsonl")
	if err != nil {
		return Agent{}, false
	}
	defer f.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil || len(r.cache) >= maxAgentCache {
		r.cache = map[string]*agentState{}
	}
	st := r.cache[base]
	if st == nil {
		st = &agentState{agent: Agent{AgentID: id}}
		var meta struct {
			Description string `json:"description"`
			AgentType   string `json:"agentType"`
			ToolUseID   string `json:"toolUseId"`
		}
		if b, err := os.ReadFile(base + ".meta.json"); err == nil && json.Unmarshal(b, &meta) == nil {
			st.agent.Description, st.agent.AgentType, st.agent.ToolUseID = meta.Description, meta.AgentType, meta.ToolUseID
		}
		r.cache[base] = st
	}
	if info, err := f.Stat(); err == nil && info.Size() < st.offset {
		st.offset, st.agent.ToolCount = 0, 0
	}
	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return st.agent, true
	}
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) == 0 || raw[len(raw)-1] != '\n' {
			break
		}
		st.offset += int64(len(raw))
		st.agent.scan(raw)
		if err != nil {
			break
		}
	}
	return st.agent, true
}

// scan updates the progress with one transcript line of the subagent.
func (a *Agent) scan(raw []byte) {
	var l line
	if json.Unmarshal(raw, &l) != nil || len(l.Message) == 0 {
		return
	}
	if !l.Timestamp.IsZero() {
		a.LastAt = l.Timestamp
	}
	if l.Type != "assistant" {
		return
	}
	var m message
	var blocks []block
	if json.Unmarshal(l.Message, &m) != nil || json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type == "tool_use" {
			a.ToolCount++
			a.LastTool, a.LastSummary = b.Name, summarize(b.Input)
		}
	}
}
