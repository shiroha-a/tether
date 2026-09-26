package fsapi

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	root, outside := setupRoot(t)
	mux := http.NewServeMux()
	(&Handler{Root: root}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, root, outside
}

func q(p string) string { return url.QueryEscape(p) }

func TestListHidesDotfilesAndSortsDirsFirst(t *testing.T) {
	srv, root, _ := newTestServer(t)
	os.WriteFile(filepath.Join(root, ".secret"), nil, 0o644)
	os.WriteFile(filepath.Join(root, "a.txt"), nil, 0o644)

	get := func(hidden string) []Entry {
		res, err := http.Get(srv.URL + "/api/fs/list?path=" + q(root) + hidden)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("list: %v %v", err, res.Status)
		}
		defer res.Body.Close()
		var body struct{ Entries []Entry }
		json.NewDecoder(res.Body).Decode(&body)
		return body.Entries
	}
	names := func(es []Entry) string {
		var s []string
		for _, e := range es {
			s = append(s, e.Name)
		}
		return strings.Join(s, ",")
	}
	if got := names(get("")); got != "escape,inner,sub,a.txt" {
		t.Fatalf("entries = %s", got)
	}
	if got := names(get("&hidden=1")); !strings.Contains(got, ".secret") {
		t.Fatalf("hidden entries = %s", got)
	}
}

func TestListOutsideRootForbidden(t *testing.T) {
	srv, root, outside := newTestServer(t)
	for _, p := range []string{outside, filepath.Join(root, "escape")} {
		res, _ := http.Get(srv.URL + "/api/fs/list?path=" + q(p))
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: status = %d", p, res.StatusCode)
		}
	}
}

func TestMkdir(t *testing.T) {
	srv, root, outside := newTestServer(t)
	post := func(p string) int {
		b, _ := json.Marshal(map[string]string{"path": p})
		res, err := http.Post(srv.URL+"/api/fs/mkdir", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode
	}
	if c := post(filepath.Join(root, "newdir")); c != http.StatusCreated {
		t.Fatalf("create: %d", c)
	}
	if c := post(filepath.Join(root, "newdir")); c != http.StatusConflict {
		t.Fatalf("duplicate: %d", c)
	}
	if c := post(filepath.Join(root, "escape", "x")); c != http.StatusForbidden {
		t.Fatalf("escape: %d", c)
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); err == nil {
		t.Fatal("directory created outside root")
	}
}

func upload(t *testing.T, srv *httptest.Server, dir, name, content, extra string) int {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write([]byte(content))
	mw.Close()
	res, err := http.Post(srv.URL+"/api/fs/upload?dir="+q(dir)+extra, mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode
}

func TestUpload(t *testing.T) {
	srv, root, outside := newTestServer(t)
	dir := filepath.Join(root, "sub")

	if c := upload(t, srv, dir, "up.txt", "hello", ""); c != http.StatusCreated {
		t.Fatalf("upload: %d", c)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "up.txt")); string(b) != "hello" {
		t.Fatalf("content = %q", b)
	}
	if c := upload(t, srv, dir, "up.txt", "again", ""); c != http.StatusConflict {
		t.Fatalf("no-overwrite: %d", c)
	}
	if c := upload(t, srv, dir, "up.txt", "again", "&overwrite=1"); c != http.StatusCreated {
		t.Fatalf("overwrite: %d", c)
	}
	// ファイル名に含まれるディレクトリ部分は捨てられ、dir直下に保存される
	if c := upload(t, srv, dir, "../../evil.txt", "x", ""); c != http.StatusCreated {
		t.Fatalf("traversal name: %d", c)
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.txt")); err != nil {
		t.Fatalf("traversal name not confined: %v", err)
	}
	if c := upload(t, srv, filepath.Join(root, "escape"), "x.txt", "x", ""); c != http.StatusForbidden {
		t.Fatalf("escape dir: %d", c)
	}
	// 既存のシンボリックリンクを上書きしてROOT外に書き込めないこと
	os.Symlink(filepath.Join(outside, "target.txt"), filepath.Join(dir, "link.txt"))
	if c := upload(t, srv, dir, "link.txt", "pwned", "&overwrite=1"); c == http.StatusCreated {
		t.Fatalf("overwrote through symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "target.txt")); err == nil {
		t.Fatal("file written outside root via symlink")
	}
}

func TestPreview(t *testing.T) {
	srv, root, _ := newTestServer(t)
	md := filepath.Join(root, "doc.md")
	os.WriteFile(md, []byte("# 見出し"), 0o644)
	bin := filepath.Join(root, "bin.dat")
	os.WriteFile(bin, []byte{0x7f, 'E', 'L', 'F', 0}, 0o644)
	big := filepath.Join(root, "big.txt")
	// 1MiB境界でマルチバイト文字が分断されるように作る
	os.WriteFile(big, []byte(strings.Repeat("a", MaxPreview-1)+"あい"), 0o644)

	res, _ := http.Get(srv.URL + "/api/fs/preview?path=" + q(md))
	var body struct {
		Kind, Content string
		Truncated     bool
	}
	json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode != 200 || body.Kind != "markdown" || body.Content != "# 見出し" {
		t.Fatalf("md preview: %d %+v", res.StatusCode, body)
	}
	if res, _ := http.Get(srv.URL + "/api/fs/preview?path=" + q(bin)); res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("binary preview: %d", res.StatusCode)
	}
	res, _ = http.Get(srv.URL + "/api/fs/preview?path=" + q(big))
	body.Content = ""
	json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode != 200 || !body.Truncated || len(body.Content) != MaxPreview-1 {
		t.Fatalf("big preview: %d truncated=%v len=%d", res.StatusCode, body.Truncated, len(body.Content))
	}
}

func TestDownloadHeaders(t *testing.T) {
	srv, root, _ := newTestServer(t)
	os.WriteFile(filepath.Join(root, "page.html"), []byte("<script>alert(1)</script>"), 0o644)
	os.WriteFile(filepath.Join(root, "pic.png"), []byte("png"), 0o644)

	res, _ := http.Get(srv.URL + "/api/fs/download?inline=1&path=" + q(filepath.Join(root, "page.html")))
	if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("html must be attachment, got %q", cd)
	}
	res, _ = http.Get(srv.URL + "/api/fs/download?inline=1&path=" + q(filepath.Join(root, "pic.png")))
	if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") || res.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("png inline: %q %q", cd, res.Header.Get("Content-Type"))
	}
	res, _ = http.Get(srv.URL + "/api/fs/download?path=" + q(filepath.Join(root, "sub")))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("dir download: %d", res.StatusCode)
	}
}

func statReq(t *testing.T, srv *httptest.Server, base string, paths []string) (int, []StatResult) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"base": base, "paths": paths})
	res, err := http.Post(srv.URL+"/api/fs/stat", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out []StatResult
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestStat(t *testing.T) {
	root, outside := setupRoot(t)
	mux := http.NewServeMux()
	(&Handler{Root: root, Home: root}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	sub := filepath.Join(root, "sub")
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644)

	code, res := statReq(t, srv, sub, []string{
		"file.txt",                     // baseからの相対
		"./file.txt",                   // 同じファイルを別表記で
		"../sub",                       // フォルダ
		filepath.Join(sub, "file.txt"), // 絶対パス
		"~/sub/file.txt",               // ホーム基準
		"missing.md",                   // 存在しない
		"../escape/secret.txt",         // シンボリックリンクでルート外へ
		outside + "/secret.txt",        // ルート外の絶対パス
		"../../../etc/passwd",          // ..でルート外へ
		"",
	})
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	got := map[string]StatResult{}
	for _, r := range res {
		got[r.Input] = r
	}
	want := filepath.Join(sub, "file.txt")
	for _, in := range []string{"file.txt", "./file.txt", filepath.Join(sub, "file.txt"), "~/sub/file.txt"} {
		if r, ok := got[in]; !ok || r.Path != want || r.IsDir || r.Size != 2 {
			t.Errorf("%q -> %+v (found %v)", in, r, ok)
		}
	}
	if r, ok := got["../sub"]; !ok || !r.IsDir || r.Path != sub {
		t.Errorf("../sub -> %+v", r)
	}
	for _, in := range []string{"missing.md", "../escape/secret.txt", outside + "/secret.txt", "../../../etc/passwd", ""} {
		if r, ok := got[in]; ok {
			t.Errorf("%q must not be returned, got %+v", in, r)
		}
	}
	if len(res) != 5 {
		t.Errorf("results = %d, want 5", len(res))
	}
}

func TestStatRejectsBaseOutsideRootAndTooManyPaths(t *testing.T) {
	root, outside := setupRoot(t)
	mux := http.NewServeMux()
	(&Handler{Root: root}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if code, _ := statReq(t, srv, outside, []string{"x"}); code != http.StatusForbidden {
		t.Fatalf("base outside root: %d", code)
	}
	many := make([]string, maxStatPaths+1)
	for i := range many {
		many[i] = "x"
	}
	if code, _ := statReq(t, srv, root, many); code != http.StatusBadRequest {
		t.Fatalf("too many paths: %d", code)
	}
}
