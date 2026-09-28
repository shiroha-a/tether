package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"tether/internal/guard"
	"tether/internal/session"
)

// pngBytes starts with the PNG signature, which is what the upload checks.
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)

func TestTranscriptImageAPI(t *testing.T) {
	e := newEnv(t)
	s, _ := e.m.Create(session.Options{Cwd: e.root})
	id := s.Spec().ID
	data := base64.StdEncoding.EncodeToString(pngBytes)
	path := filepath.Join(t.TempDir(), "c.jsonl")
	os.WriteFile(path, []byte(transcriptLine("u1", "user",
		`[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+data+`"}},{"type":"text","text":"見て"}]`)), 0o644)
	e.transcripts[s.Spec().ClaudeSessionID] = path

	// 記録の画像の参照を、そのまま画像のURLに使える
	res := e.do(t, "GET", "/api/sessions/"+id+"/transcript", nil, true)
	var chunk struct {
		Items []struct {
			Images []struct{ Line, Index, Sub int } `json:"images"`
		} `json:"items"`
	}
	json.NewDecoder(res.Body).Decode(&chunk)
	if len(chunk.Items) != 1 || len(chunk.Items[0].Images) != 1 {
		t.Fatalf("items = %+v", chunk.Items)
	}
	res = e.do(t, "GET", "/api/sessions/"+id+"/transcript/image?line=0&index=0&sub=-1", nil, true)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/png" ||
		res.Header.Get("X-Content-Type-Options") != "nosniff" || !bytes.Equal(body, pngBytes) {
		t.Fatalf("image: %d %q %q", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
	for q, want := range map[string]int{
		"line=0&index=1&sub=-1": http.StatusNotFound,
		"line=3&index=0&sub=-1": http.StatusNotFound,
		"line=0&index=0":        http.StatusBadRequest,
		"line=x&index=0&sub=-1": http.StatusBadRequest,
	} {
		if c := e.do(t, "GET", "/api/sessions/"+id+"/transcript/image?"+q, nil, true).StatusCode; c != want {
			t.Errorf("%s: %d, want %d", q, c, want)
		}
	}
	sh, _ := e.m.Create(session.Options{Cwd: e.root, Kind: session.KindShell})
	if c := e.do(t, "GET", "/api/sessions/"+sh.Spec().ID+"/transcript/image?line=0&index=0&sub=-1", nil, true).StatusCode; c != http.StatusBadRequest {
		t.Fatalf("shell: %d", c)
	}
	if c := e.do(t, "GET", "/api/sessions/"+id+"/transcript/image?line=0&index=0&sub=-1", nil, false).StatusCode; c != http.StatusUnauthorized {
		t.Fatalf("without token: %d", c)
	}
}

func TestSessionAgentsAPI(t *testing.T) {
	e := newEnv(t)
	s, _ := e.m.Create(session.Options{Cwd: e.root})
	id := s.Spec().ID
	dir := t.TempDir()
	path := filepath.Join(dir, "c.jsonl")
	os.WriteFile(path, nil, 0o644)
	e.transcripts[s.Spec().ClaudeSessionID] = path
	os.MkdirAll(filepath.Join(dir, "c", "subagents"), 0o755)
	os.WriteFile(filepath.Join(dir, "c", "subagents", "agent-a1.meta.json"), []byte(`{"description":"調査","toolUseId":"toolu_1"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "c", "subagents", "agent-a1.jsonl"),
		[]byte(transcriptLine("x", "assistant", `[{"type":"tool_use","id":"t","name":"Grep","input":{"pattern":"TODO"}}]`)), 0o644)

	res := e.do(t, "GET", "/api/sessions/"+id+"/agents?toolUseIds=toolu_1,zz", nil, true)
	var agents []map[string]any
	json.NewDecoder(res.Body).Decode(&agents)
	if res.StatusCode != http.StatusOK || len(agents) != 1 || agents[0]["description"] != "調査" || agents[0]["lastTool"] != "Grep" {
		t.Fatalf("agents: %d %+v", res.StatusCode, agents)
	}
}

func upload(t *testing.T, e *env, id, name string, data []byte) *http.Response {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/sessions/"+id+"/images", &b)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(guard.CSRFHeader, "1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestImageUploadAndInput(t *testing.T) {
	e := newEnv(t)
	s, _ := e.m.Create(session.Options{Cwd: e.root})
	id := s.Spec().ID

	res := upload(t, e, id, "shot.png", pngBytes)
	var got struct{ ID string }
	json.NewDecoder(res.Body).Decode(&got)
	if res.StatusCode != http.StatusOK || !uploadName.MatchString(got.ID) || !strings.HasSuffix(got.ID, ".png") {
		t.Fatalf("upload: %d %+v", res.StatusCode, got)
	}
	saved := filepath.Join(e.uploads, id, got.ID)
	if info, err := os.Stat(saved); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved file: %v %v", info, err)
	}

	// 形式は名前ではなく中身で判定する
	if c := upload(t, e, id, "evil.png", []byte("<svg onload=alert(1)>")).StatusCode; c != http.StatusBadRequest {
		t.Fatalf("non-image: %d", c)
	}
	if c := upload(t, e, id, "big.png", append(pngBytes, make([]byte, maxImageUpload)...)).StatusCode; c != http.StatusBadRequest {
		t.Fatalf("too large: %d", c)
	}
	if c := upload(t, e, "nope", "a.png", pngBytes).StatusCode; c != http.StatusNotFound {
		t.Fatalf("unknown session: %d", c)
	}

	// 画像のパスはbracketed pasteで1つずつ貼り、その後に文章とEnter（catはそのままエコーする）
	post := func(body map[string]any) int {
		t.Helper()
		return e.do(t, "POST", "/api/sessions/"+id+"/input", body, true).StatusCode
	}
	if c := post(map[string]any{"text": "what is this", "images": []string{got.ID}}); c != http.StatusNoContent {
		t.Fatalf("input with image: %d", c)
	}
	waitOutput(t, s, "what is this")
	c, replay := s.Attach(80, 24)
	s.Detach(c)
	if !strings.Contains(string(replay), "\x1b[200~"+saved+"\x1b[201~") && !strings.Contains(string(replay), "^[[200~"+saved+"^[[201~") {
		t.Fatalf("image path was not pasted: %q", replay)
	}
	// 画像だけでも送れる
	if c := post(map[string]any{"images": []string{got.ID}}); c != http.StatusNoContent {
		t.Fatalf("image only: %d", c)
	}

	other, _ := e.m.Create(session.Options{Cwd: e.root})
	os.WriteFile(filepath.Join(e.root, "x.png"), pngBytes, 0o644)
	for name, body := range map[string]map[string]any{
		"traversal":     {"text": "x", "images": []string{"../" + id + "/" + got.ID}},
		"absolute path": {"text": "x", "images": []string{filepath.Join(e.root, "x.png")}},
		"unknown id":    {"text": "x", "images": []string{strings.Repeat("0", 32) + ".png"}},
		"with keys":     {"key": "enter", "images": []string{got.ID}},
		"too many":      {"text": "x", "images": slices.Repeat([]string{got.ID}, maxInputImages+1)},
	} {
		if c := post(body); c != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, c)
		}
	}
	// 別のセッションに上げた画像は使えない
	if c := e.do(t, "POST", "/api/sessions/"+other.Spec().ID+"/input", map[string]any{"text": "x", "images": []string{got.ID}}, true).StatusCode; c != http.StatusBadRequest {
		t.Fatalf("other session's image: %d", c)
	}

	// 古い画像は次のアップロードで消える
	old := time.Now().Add(-uploadTTL - time.Hour)
	os.Chtimes(saved, old, old)
	upload(t, e, id, "next.png", pngBytes)
	if _, err := os.Stat(saved); !os.IsNotExist(err) {
		t.Fatalf("old upload was kept: %v", err)
	}
}
