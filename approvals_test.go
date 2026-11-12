package main

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// rigWithRoles writes "nick sha256 role" lines, so owner and guest exist.
func rigWithRoles(t *testing.T, roles map[string]string) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{t: t, tokens: map[string]string{}, dir: dir}
	var lines []string
	for nick, role := range roles {
		tok, line, err := NewToken(nick)
		if err != nil {
			t.Fatal(err)
		}
		r.tokens[nick] = tok
		lines = append(lines, line+" "+role)
	}
	path := filepath.Join(dir, "tokens.txt")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.start(path)
	return r
}

var askID = regexp.MustCompile(`asks #(\d+):`)

func askOf(t *testing.T, text string) string {
	t.Helper()
	m := askID.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no ask id in %q", text)
	}
	return m[1]
}

func TestOnlyTheOwnerDecidesAnAskOnce(t *testing.T) {
	r := rigWithRoles(t, map[string]string{"alice": "agent", "boss": "owner"})
	text, isErr := r.call("alice", "ask", map[string]any{"question": "Deploy the migration now?"})
	if isErr {
		t.Fatalf("ask: %s", text)
	}
	id := askOf(t, text)

	if out, isErr := r.call("alice", "answer", map[string]any{"id": id, "decision": "approve"}); !isErr {
		t.Fatalf("an agent answered its own ask: %s", out)
	}
	out, isErr := r.call("boss", "answer", map[string]any{"id": id, "decision": "deny", "note": "after the backup"})
	if isErr || !strings.Contains(out, "denied ask #"+id+": after the backup") {
		t.Fatalf("owner answer = %q, %v", out, isErr)
	}
	if out, isErr := r.call("boss", "answer", map[string]any{"id": id, "decision": "approve"}); !isErr {
		t.Fatalf("an ask was decided twice: %s", out)
	}
	// The decision is in the log, next to the ask.
	hist, _ := r.call("alice", "history", map[string]any{})
	if !strings.Contains(hist, "?? <alice> asks #"+id) || !strings.Contains(hist, "!! boss denied ask #"+id) {
		t.Errorf("history:\n%s", hist)
	}
}

func TestAwaitAnswerWakesOnTheDecision(t *testing.T) {
	r := rigWithRoles(t, map[string]string{"alice": "agent", "boss": "owner"})
	text, _ := r.call("alice", "ask", map[string]any{"question": "Merge PR 12?"})
	id := askOf(t, text)
	got := make(chan string, 1)
	go func() {
		out, _ := r.call("alice", "await_answer", map[string]any{"id": id, "wait_seconds": 10})
		got <- out
	}()
	time.Sleep(200 * time.Millisecond)
	r.call("boss", "say", map[string]any{"text": "unrelated line"})
	r.call("boss", "answer", map[string]any{"id": id, "decision": "approve"})
	select {
	case out := <-got:
		if !strings.Contains(out, "boss approved ask #"+id) {
			t.Errorf("await_answer = %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("await_answer did not wake up")
	}
	// Already decided: answers at once, without waiting.
	if out, _ := r.call("alice", "await_answer", map[string]any{"id": id}); !strings.Contains(out, "approved") {
		t.Errorf("second await_answer = %q", out)
	}
}

func TestGuestsReadButNeverWrite(t *testing.T) {
	r := rigWithRoles(t, map[string]string{"alice": "agent", "visitor": "guest"})
	r.call("alice", "say", map[string]any{"text": "hello"})
	for tool, args := range map[string]map[string]any{
		"say":   {"text": "hi"},
		"ask":   {"question": "may I?"},
		"topic": {"text": "new topic"},
		"claim": {"paths": []string{"src/"}},
	} {
		if out, isErr := r.call("visitor", tool, args); !isErr {
			t.Errorf("guest %s was allowed: %s", tool, out)
		}
	}
	if hist, isErr := r.call("visitor", "history", map[string]any{}); isErr || !strings.Contains(hist, "<alice> hello") {
		t.Errorf("guest cannot read: %q", hist)
	}
	if out, isErr := r.call("visitor", "topic", map[string]any{}); isErr {
		t.Errorf("guest cannot read the topic: %q", out)
	}
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/irc/api/say", strings.NewReader(`{"text":"hi"}`))
	req.SetBasicAuth("x", r.tokens["visitor"])
	req.Header.Set("X-AgentIRC", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("guest page post = %d, want 403", resp.StatusCode)
	}
}

func TestOwnerAnswersFromThePage(t *testing.T) {
	r := rigWithRoles(t, map[string]string{"alice": "agent", "boss": "owner"})
	text, _ := r.call("alice", "ask", map[string]any{"question": "Ship it?"})
	id := askOf(t, text)
	post := func(nick, body string) int {
		req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/irc/api/say", strings.NewReader(body))
		req.SetBasicAuth("x", r.tokens[nick])
		req.Header.Set("X-AgentIRC", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("alice", `{"text":"/approve `+id+`"}`); code != http.StatusForbidden {
		t.Errorf("agent /approve = %d, want 403", code)
	}
	if code := post("boss", `{"text":"/approve #`+id+` go"}`); code != http.StatusNoContent {
		t.Errorf("owner /approve = %d, want 204", code)
	}
	if out, _ := r.call("alice", "await_answer", map[string]any{"id": id}); !strings.Contains(out, "boss approved ask #"+id+": go") {
		t.Errorf("await_answer after page approve = %q", out)
	}
}

func TestTokensFileRoles(t *testing.T) {
	_, line, _ := NewToken("x")
	path := filepath.Join(t.TempDir(), "t.txt")
	_ = os.WriteFile(path, []byte(line+" king\n"), 0o600)
	if _, err := LoadTokens(path); err == nil {
		t.Error("an unknown role was accepted")
	}
	_ = os.WriteFile(path, []byte(line+"\n"), 0o600)
	tokens, err := LoadTokens(path)
	if err != nil || tokens.Role("x") != RoleAgent {
		t.Errorf("no role column = %v, %q; want agent", err, tokens.Role("x"))
	}
}
