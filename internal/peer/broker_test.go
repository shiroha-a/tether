package peer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// waitPending waits until the broker has n pending approvals and returns them.
func waitPending(t *testing.T, b *Broker, n int) []Approval {
	t.Helper()
	for range 200 {
		if p := b.Pending(); len(p) == n {
			return p
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pending approvals never reached %d (now %d)", n, len(b.Pending()))
	return nil
}

func TestBrokerDecisions(t *testing.T) {
	b := NewBroker(time.Minute)
	var changes, requests atomic.Int32
	b.OnChange = func() { changes.Add(1) }
	b.OnRequest = func(Approval) { requests.Add(1) }

	ask := func(a Approval) chan Outcome {
		ch := make(chan Outcome, 1)
		go func() { ch <- b.Ask(context.Background(), a) }()
		return ch
	}
	// 許可
	out := ask(Approval{Session: "s1", Machine: "B", Op: "exec", Detail: "ls"})
	p := waitPending(t, b, 1)[0]
	if p.ID == "" || p.Detail != "ls" || p.ExpiresAt.Sub(p.CreatedAt) != time.Minute || requests.Load() != 1 {
		t.Fatalf("pending = %+v requests=%d", p, requests.Load())
	}
	if !b.Decide(p.ID, Decision{Allow: true, GrantMinutes: 15}) {
		t.Fatal("decide failed")
	}
	if o := <-out; o != Allowed {
		t.Fatalf("outcome = %v", o)
	}
	// コマンド実行はまとめて許可できない
	if g := b.Grants(); len(g) != 0 {
		t.Fatalf("exec produced a grant: %+v", g)
	}
	if b.Decide(p.ID, Decision{Allow: true}) {
		t.Fatal("decided twice")
	}
	waitPending(t, b, 0)

	// 同じ承認を続けて2回決めても、2回目は受け付けない（二度押し）
	out = ask(Approval{Session: "s1", Machine: "B", Op: "exec"})
	p = waitPending(t, b, 1)[0]
	first := b.Decide(p.ID, Decision{Allow: false})
	second := make(chan bool, 1)
	go func() { second <- b.Decide(p.ID, Decision{Allow: true}) }()
	select {
	case ok := <-second:
		if !first || ok {
			t.Fatalf("double decide: first=%v second=%v", first, ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second decide blocked")
	}
	if o := <-out; o != Denied {
		t.Fatalf("double decide outcome = %v", o)
	}

	// 拒否
	out = ask(Approval{Session: "s1", Machine: "B", Op: "read", Read: true})
	p = waitPending(t, b, 1)[0]
	b.Decide(p.ID, Decision{Allow: false, GrantMinutes: 15})
	if o := <-out; o != Denied {
		t.Fatalf("deny outcome = %v", o)
	}
	if g := b.Grants(); len(g) != 0 {
		t.Fatalf("deny produced a grant: %+v", g)
	}
	if changes.Load() < 4 {
		t.Fatalf("OnChange called %d times", changes.Load())
	}
}

func TestBrokerReadGrant(t *testing.T) {
	b := NewBroker(time.Minute)
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	out := make(chan Outcome, 1)
	go func() {
		out <- b.Ask(context.Background(), Approval{Session: "s1", Machine: "B", Op: "status", Read: true})
	}()
	p := waitPending(t, b, 1)[0]
	b.Decide(p.ID, Decision{Allow: true, GrantMinutes: 15})
	<-out
	g := b.Grants()
	if len(g) != 1 || g[0].Session != "s1" || g[0].Machine != "B" || !g[0].ExpiresAt.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("grants = %+v", g)
	}
	ctx := context.Background()
	// 同じセッション・同じマシンの読み取りは、聞かずに許可
	if o := b.Ask(ctx, Approval{Session: "s1", Machine: "B", Op: "read", Read: true}); o != Allowed {
		t.Fatalf("granted read = %v", o)
	}
	if len(b.Pending()) != 0 {
		t.Fatal("granted read asked the user")
	}
	// 書き込み系、別のセッション、別のマシンは聞く
	for _, a := range []Approval{
		{Session: "s1", Machine: "B", Op: "exec"},
		{Session: "s2", Machine: "B", Op: "read", Read: true},
		{Session: "s1", Machine: "C", Op: "read", Read: true},
	} {
		c, cancel := context.WithCancel(ctx)
		done := make(chan Outcome, 1)
		go func() { done <- b.Ask(c, a) }()
		waitPending(t, b, 1)
		cancel()
		if o := <-done; o != Canceled {
			t.Fatalf("%+v: outcome %v", a, o)
		}
		waitPending(t, b, 0)
	}
	// 期限が切れたら聞く（一覧から消す処理とは別に、期限そのものを見ている）
	now = now.Add(15 * time.Minute)
	c, cancel := context.WithCancel(ctx)
	done := make(chan Outcome, 1)
	go func() { done <- b.Ask(c, Approval{Session: "s1", Machine: "B", Op: "read", Read: true}) }()
	waitPending(t, b, 1)
	cancel()
	<-done
	if len(b.Grants()) != 0 {
		t.Fatal("expired grant listed")
	}
}

func TestBrokerGrantLimitRevokeAndDrop(t *testing.T) {
	b := NewBroker(time.Minute)
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	grant := func(session string, minutes int) {
		out := make(chan Outcome, 1)
		go func() { out <- b.Ask(context.Background(), Approval{Session: session, Machine: "B", Read: true}) }()
		p := waitPending(t, b, 1)[0]
		b.Decide(p.ID, Decision{Allow: true, GrantMinutes: minutes})
		<-out
	}
	grant("s1", 24*60)
	if g := b.Grants(); len(g) != 1 || !g[0].ExpiresAt.Equal(now.Add(MaxGrant)) {
		t.Fatalf("grant not capped: %+v", g)
	}
	grant("s2", 5)
	id := b.Grants()[0].ID
	if !b.Revoke(id) || b.Revoke(id) {
		t.Fatal("revoke")
	}
	b.DropSession("s2")
	if g := b.Grants(); len(g) != 0 {
		t.Fatalf("grants after drop = %+v", g)
	}
}

func TestBrokerTimeout(t *testing.T) {
	b := NewBroker(30 * time.Millisecond)
	if o := b.Ask(context.Background(), Approval{Session: "s", Machine: "B"}); o != TimedOut {
		t.Fatalf("outcome = %v", o)
	}
	if len(b.Pending()) != 0 {
		t.Fatal("timed out approval still pending")
	}
}

// fakeRemote runs a Service and returns a Store on the calling side that knows it.
func fakeRemote(t *testing.T, p Policy) (*Store, *serviceEnv) {
	t.Helper()
	e := newServiceEnv(t, p)
	local, _ := NewStore(t.TempDir())
	if _, err := local.AddRemote("B", "http://localhost:1", e.token, p); err != nil {
		t.Fatal(err)
	}
	// httptestは127.0.0.1の任意のポートなので、URLは保存後に差し替える
	local.mu.Lock()
	local.data.Remotes[0].URL = e.srv.URL
	local.mu.Unlock()
	return local, e
}

func TestToolsCall(t *testing.T) {
	local, e := fakeRemote(t, allowAll)
	b := NewBroker(time.Minute)
	tools := &Tools{Store: local, Broker: b, Caller: &Caller{}}
	ctx := context.Background()

	// 接続先の一覧は承認なし
	res := tools.Call(ctx, "s1", "proj", "remote_machines", nil)
	if res.IsError || !strings.Contains(res.Text, `"name": "B"`) || len(b.Pending()) != 0 {
		t.Fatalf("machines: %+v", res)
	}

	call := func(name, args string, d *Decision) (ToolResult, Approval) {
		t.Helper()
		out := make(chan ToolResult, 1)
		go func() { out <- tools.Call(ctx, "s1", "proj", name, json.RawMessage(args)) }()
		var a Approval
		if d != nil {
			a = waitPending(t, b, 1)[0]
			b.Decide(a.ID, *d)
		}
		return <-out, a
	}
	res, a := call("remote_exec", `{"machine":"B","command":"echo hi","cwd":"sub"}`, &Decision{Allow: true})
	if a.Op != "exec" || a.Read || a.Machine != "B" || a.SessionLabel != "proj" || a.Detail != "echo hi\n(in sub)" {
		t.Fatalf("approval = %+v", a)
	}
	if res.IsError || !strings.Contains(res.Text, "untrusted") || !strings.Contains(res.Text, `"output": "hi\n"`) {
		t.Fatalf("exec result = %+v", res)
	}
	// 拒否したら実行しない
	res, _ = call("remote_exec", `{"machine":"B","command":"touch denied"}`, &Decision{Allow: false})
	if !res.IsError || !strings.Contains(res.Text, "denied") {
		t.Fatalf("deny result = %+v", res)
	}
	if code, _, _ := e.do(t, "GET", "/peer/v1/file?path=denied", e.token, nil); code != 404 {
		t.Fatal("denied command ran")
	}
	// 読み取りをまとめて許可すると、次の読み取りは聞かれない
	res, a = call("remote_read_file", `{"machine":"B","path":"sub/a.txt"}`, &Decision{Allow: true, GrantMinutes: 15})
	if !a.Read || a.Op != "read" || res.IsError || !strings.Contains(res.Text, `"content": "hello"`) {
		t.Fatalf("read: %+v %+v", a, res)
	}
	res, _ = call("remote_list_files", `{"machine":"B"}`, nil)
	if res.IsError || !strings.Contains(res.Text, `"name": "sub"`) {
		t.Fatalf("granted list: %+v", res)
	}
	// 接続先が断った場合（ルートの外）
	res, _ = call("remote_read_file", `{"machine":"B","path":"../x"}`, nil)
	if !res.IsError || !strings.Contains(res.Text, "403") {
		t.Fatalf("remote refusal: %+v", res)
	}
	// 知らないマシン・ツール・壊れた引数は、承認を求めずにエラー
	var asked atomic.Int32
	b.OnRequest = func(Approval) { asked.Add(1) }
	for _, c := range [][2]string{{"remote_status", `{"machine":"Z"}`}, {"remote_nope", `{"machine":"B"}`}, {"remote_status", `{`}} {
		short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		res := tools.Call(short, "s1", "proj", c[0], json.RawMessage(c[1]))
		cancel()
		if !res.IsError || asked.Load() != 0 {
			t.Fatalf("%v: %+v (asked %d)", c, res, asked.Load())
		}
	}
	b.OnRequest = nil
	// 依頼と結果
	res, a = call("remote_delegate", `{"machine":"B","prompt":"do it"}`, &Decision{Allow: true})
	if a.Op != "delegate" || a.Read || res.IsError || !strings.Contains(res.Text, "sess-1") {
		t.Fatalf("delegate: %+v %+v", a, res)
	}
	res, _ = call("remote_delegate_result", `{"machine":"B","session":"sess-1"}`, nil)
	if res.IsError || !strings.Contains(res.Text, "PONG") {
		t.Fatalf("delegate result (granted read): %+v", res)
	}
}

func TestToolsUnreachableRemote(t *testing.T) {
	local, _ := NewStore(t.TempDir())
	local.AddRemote("B", "http://localhost:1", "tok", allowAll)
	b := NewBroker(time.Minute)
	tools := &Tools{Store: local, Broker: b, Caller: &Caller{}}
	out := make(chan ToolResult, 1)
	go func() {
		out <- tools.Call(context.Background(), "s", "p", "remote_status", json.RawMessage(`{"machine":"B"}`))
	}()
	b.Decide(waitPending(t, b, 1)[0].ID, Decision{Allow: true})
	if res := <-out; !res.IsError || !strings.Contains(res.Text, "could not reach") {
		t.Fatalf("unreachable: %+v", res)
	}
}

func TestCallerHelloAndErrors(t *testing.T) {
	e := newServiceEnv(t, Policy{Status: true})
	c := &Caller{}
	h, err := c.Hello(context.Background(), e.srv.URL, e.token)
	if err != nil || h.Client != "A" || !h.Policy.Status || h.Policy.Exec {
		t.Fatalf("hello: %+v %v", h, err)
	}
	_, err = c.Hello(context.Background(), e.srv.URL, "wrong")
	var re *RemoteError
	if !errors.As(err, &re) || re.Status != 401 {
		t.Fatalf("bad token: %v", err)
	}
}

type fakeBackend struct {
	release chan struct{}
	calls   atomic.Int32
}

func (f *fakeBackend) ListTools(context.Context) ([]Tool, error) { return ToolList[:1], nil }

func (f *fakeBackend) CallTool(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
	f.calls.Add(1)
	switch name {
	case "slow":
		select {
		case <-f.release:
			return ToolResult{Text: "slow done"}, nil
		case <-ctx.Done():
			return ToolResult{Text: "canceled", IsError: true}, nil
		}
	case "broken":
		return ToolResult{}, errors.New("connection refused")
	}
	return ToolResult{Text: name + string(args)}, nil
}

func TestServeMCP(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	backend := &fakeBackend{release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- ServeMCP(context.Background(), inR, outW, backend, "v9"); outW.Close() }()
	responses := make(chan map[string]any, 10)
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var v map[string]any
			json.Unmarshal(sc.Bytes(), &v)
			responses <- v
		}
		close(responses)
	}()
	send := func(s string) { inW.Write([]byte(s + "\n")) }
	next := func() map[string]any {
		select {
		case v := <-responses:
			return v
		case <-time.After(2 * time.Second):
			t.Fatal("no response")
			return nil
		}
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	r := next()
	res := r["result"].(map[string]any)
	if r["id"] != float64(1) || res["protocolVersion"] != "2025-03-26" || res["serverInfo"].(map[string]any)["version"] != "v9" {
		t.Fatalf("initialize = %v", r)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if r := next(); len(r["result"].(map[string]any)["tools"].([]any)) != 1 {
		t.Fatalf("tools/list = %v", r)
	}
	// 承認待ちで長くかかる呼び出しの間も、ほかの要求に答える
	send(`{"jsonrpc":"2.0","id":"slow-1","method":"tools/call","params":{"name":"slow"}}`)
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"a":1}}}`)
	r = next()
	content := r["result"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if r["id"] != float64(3) || content["text"] != `echo{"a":1}` || r["result"].(map[string]any)["isError"] != false {
		t.Fatalf("echo = %v", r)
	}
	// 取り消しの通知で、実行中の呼び出しを止める
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"slow-1"}}`)
	r = next()
	if r["id"] != "slow-1" || r["result"].(map[string]any)["isError"] != true {
		t.Fatalf("canceled = %v", r)
	}
	send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"broken"}}`)
	r = next()
	if r["result"].(map[string]any)["isError"] != true || !strings.Contains(r["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), "not reachable") {
		t.Fatalf("broken = %v", r)
	}
	send(`{"jsonrpc":"2.0","id":5,"method":"nope"}`)
	if r := next(); r["error"].(map[string]any)["code"] != float64(-32601) {
		t.Fatalf("unknown method = %v", r)
	}
	send(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{}}`)
	if r := next(); r["error"].(map[string]any)["code"] != float64(-32602) {
		t.Fatalf("missing name = %v", r)
	}
	send(`{broken`)
	if r := next(); r["error"].(map[string]any)["code"] != float64(-32700) {
		t.Fatalf("parse error = %v", r)
	}
	send(`{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	if r := next(); r["id"] != float64(7) || r["error"] != nil {
		t.Fatalf("ping = %v", r)
	}
	inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
