package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type rig struct {
	t      *testing.T
	srv    *httptest.Server
	store  *Store
	tokens map[string]string // nick -> token
	dir    string
	// session holds each nick's Mcp-Session-Id once it has initialized,
	// and is sent back on every later request, as a real client does.
	session map[string]string
}

func newRig(t *testing.T, nicks ...string) *rig {
	t.Helper()
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "tokens.txt")
	r := &rig{t: t, tokens: map[string]string{}, dir: dir}
	var lines []string
	for _, n := range nicks {
		tok, line, err := NewToken(n)
		if err != nil {
			t.Fatal(err)
		}
		r.tokens[n] = tok
		lines = append(lines, line)
	}
	if err := os.WriteFile(tokPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.start(tokPath)
	return r
}

func (r *rig) start(tokPath string) {
	tokens, err := LoadTokens(tokPath)
	if err != nil {
		r.t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(r.dir, "data"))
	if err != nil {
		r.t.Fatal(err)
	}
	r.store = store
	r.srv = httptest.NewServer(NewServer(store, tokens).Handler("/irc"))
	r.t.Cleanup(func() { r.srv.Close(); store.Close() })
}

// rpc posts one JSON-RPC request to /irc/mcp as nick and returns the result.
func (r *rig) rpc(nick, method string, params any) (int, map[string]any) {
	r.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/irc/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if nick != "" {
		req.Header.Set("Authorization", "Bearer "+r.tokens[nick])
		if id := r.session[nick]; id != "" {
			req.Header.Set("Mcp-Session-Id", id)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" && nick != "" {
		if r.session == nil {
			r.session = map[string]string{}
		}
		r.session[nick] = id
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// call runs a tool and returns its text and isError.
func (r *rig) call(nick, tool string, args map[string]any) (string, bool) {
	r.t.Helper()
	_, out := r.rpc(nick, "tools/call", map[string]any{"name": tool, "arguments": args})
	res, _ := out["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		r.t.Fatalf("%s: no content in %v", tool, out)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

func TestMCPHandshakeAndTools(t *testing.T) {
	r := newRig(t, "win", "mac")
	code, out := r.rpc("win", "initialize", map[string]any{"protocolVersion": "2025-03-26"})
	if code != 200 || out["result"].(map[string]any)["protocolVersion"] != "2025-03-26" {
		t.Fatalf("initialize = %d %v", code, out)
	}
	_, out = r.rpc("win", "tools/list", nil)
	if n := len(out["result"].(map[string]any)["tools"].([]any)); n != len(tools) {
		t.Errorf("tools = %d, want %d", n, len(tools))
	}
	if code, _ := r.rpc("", "tools/list", nil); code != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", code)
	}
	r.tokens["eve"] = "airc_guessed"
	if code, _ := r.rpc("eve", "tools/list", nil); code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", code)
	}
}

// The point of the whole thing: one session waits, the other speaks, the
// waiting one wakes up with the line, attributed by token, not by claim.
func TestListenWakesOnReply(t *testing.T) {
	r := newRig(t, "win", "mac")
	r.call("mac", "listen", map[string]any{}) // mac catches up first
	got := make(chan string, 1)
	go func() {
		text, _ := r.call("mac", "listen", map[string]any{"wait_seconds": 10})
		got <- text
	}()
	time.Sleep(200 * time.Millisecond)
	if text, isErr := r.call("win", "say", map[string]any{"text": "cert pushed, 7114ed9"}); isErr || !strings.Contains(text, "<win> cert pushed") {
		t.Fatalf("say = %q, %v", text, isErr)
	}
	select {
	case text := <-got:
		if !strings.Contains(text, "<win> cert pushed, 7114ed9") {
			t.Errorf("listen = %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listen did not wake up")
	}
	// Already seen: nothing new now.
	if text, _ := r.call("mac", "listen", map[string]any{}); !strings.Contains(text, "nothing new") {
		t.Errorf("second listen = %q, want nothing new", text)
	}
}

// Regression: saying something must not mark unread lines as read. mac
// writes while win is busy, win then says something without listening
// first, and win's next listen must still show mac's line.
func TestSayDoesNotSkipUnreadLines(t *testing.T) {
	r := newRig(t, "win", "mac")
	r.call("win", "listen", map[string]any{})
	r.call("mac", "say", map[string]any{"text": "a question for win"})
	r.call("win", "say", map[string]any{"text": "status update"})
	text, _ := r.call("win", "listen", map[string]any{})
	if !strings.Contains(text, "<mac> a question for win") {
		t.Fatalf("mac's line was skipped:\n%s", text)
	}
}

func TestToolsBehave(t *testing.T) {
	r := newRig(t, "win")
	r.call("win", "say", map[string]any{"text": "deploying", "action": true})
	r.call("win", "topic", map[string]any{"text": "release planning"})
	hist, _ := r.call("win", "history", map[string]any{"lines": 10})
	for _, want := range []string{"*** win has joined #general", "* win deploying", "changes topic to 'release planning'"} {
		if !strings.Contains(hist, want) {
			t.Errorf("history lacks %q:\n%s", want, hist)
		}
	}
	if who, _ := r.call("win", "who", nil); !strings.Contains(who, "win") || !strings.Contains(who, "release planning") {
		t.Errorf("who = %q", who)
	}
	if _, isErr := r.call("win", "say", map[string]any{"channel": "bad channel!", "text": "x"}); !isErr {
		t.Error("an invalid channel was accepted")
	}
	if _, isErr := r.call("win", "say", map[string]any{"text": strings.Repeat("a", maxText+1)}); !isErr {
		t.Error("an oversized line was accepted")
	}
}

func TestHistorySurvivesRestart(t *testing.T) {
	r := newRig(t, "win")
	r.call("win", "say", map[string]any{"text": "before restart"})
	r.srv.Close()
	r.store.Close()
	r.start(filepath.Join(r.dir, "tokens.txt"))
	hist, _ := r.call("win", "history", map[string]any{})
	if !strings.Contains(hist, "before restart") || strings.Count(hist, "has joined") != 1 {
		t.Errorf("after restart:\n%s", hist)
	}
}

func TestBrowserSayNeedsHeader(t *testing.T) {
	r := newRig(t, "owner")
	post := func(withHeader bool) int {
		req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/irc/api/say",
			strings.NewReader(`{"channel":"#general","text":"/me waves"}`))
		req.SetBasicAuth("owner", r.tokens["owner"])
		if withHeader {
			req.Header.Set("X-AgentIRC", "1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(false); code != http.StatusForbidden {
		t.Errorf("without the header = %d, want 403 (a cross-site form must not post)", code)
	}
	if code := post(true); code != http.StatusNoContent {
		t.Errorf("with the header = %d, want 204", code)
	}
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/irc/api/lines?channel=general", nil)
	req.SetBasicAuth("x", r.tokens["owner"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out linesJSON
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Lines) != 2 || out.Lines[1].Kind != "action" || out.Me != "owner" {
		t.Errorf("lines = %+v", out)
	}
}

func TestPageNeedsLogin(t *testing.T) {
	r := newRig(t, "owner")
	resp, err := http.Get(r.srv.URL + "/irc/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic") {
		t.Errorf("page without login = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
}

func TestTokensFile(t *testing.T) {
	if _, _, err := NewToken("Not OK"); err == nil {
		t.Error("an invalid nick was accepted")
	}
	tok, line, _ := NewToken("mac")
	if strings.Contains(line, tok) {
		t.Fatal("the tokens file line contains the token itself")
	}
	path := filepath.Join(t.TempDir(), "t.txt")
	_ = os.WriteFile(path, []byte("# comment\n"+line+"\n"), 0o600)
	tokens, err := LoadTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	if nick, ok := tokens.Nick(tok); !ok || nick != "mac" {
		t.Errorf("Nick() = %q, %v", nick, ok)
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(2, time.Second)
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	if !l.allow("a") || !l.allow("a") || l.allow("a") {
		t.Fatal("burst of 2 not enforced")
	}
	now = now.Add(1100 * time.Millisecond)
	if !l.allow("a") {
		t.Error("did not refill")
	}
	if !l.allow("b") {
		t.Error("nicks share a bucket")
	}
}
