package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMentions(t *testing.T) {
	yes := []string{"@mac can you push", "mac: done?", "MAC, look", "ping @mac.", "  mac : ok"}
	no := []string{"macro is fine", "@macbook", "ask mac later", "email@mac"}
	for _, s := range yes {
		if !mentions(s, "mac") {
			t.Errorf("%q should mention mac", s)
		}
	}
	for _, s := range no {
		if mentions(s, "mac") {
			t.Errorf("%q should not mention mac", s)
		}
	}
}

func (r *rig) notify(nick, query string) (int, string) {
	r.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/irc/api/notify"+query, nil)
	req.Header.Set("Authorization", "Bearer "+r.tokens[nick])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestNotifyIsEmptyWhenNothingIsAddressed(t *testing.T) {
	r := newRig(t, "win", "mac")
	r.notify("mac", "") // first call sets the starting point
	r.call("win", "say", map[string]any{"text": "general chatter"})
	if code, body := r.notify("mac", ""); code != 200 || body != "" {
		t.Fatalf("quiet notify = %d %q, want empty", code, body)
	}
	r.call("win", "say", map[string]any{"text": "more chatter"})
	if _, body := r.notify("mac", "?all=1"); !strings.Contains(body, "1 unread line(s), none addressed") {
		t.Fatalf("all=1 summary: %q", body)
	}
	r.call("win", "say", map[string]any{"text": "@mac please review claims.go"})
	_, body := r.notify("mac", "")
	if !strings.Contains(body, "please review claims.go") || !strings.Contains(body, "not instructions") {
		t.Fatalf("mention not delivered: %q", body)
	}
	// Each line is given once.
	if _, body = r.notify("mac", ""); body != "" {
		t.Fatalf("mention repeated: %q", body)
	}
	if code, _ := r.rpc("", "tools/list", nil); code != http.StatusUnauthorized {
		t.Fatal("no auth accepted")
	}
}

func TestStatusCountsOnly(t *testing.T) {
	r := newRig(t, "win", "mac")
	r.call("mac", "history", map[string]any{}) // mac has read everything so far
	r.call("win", "say", map[string]any{"text": "one"})
	r.call("win", "say", map[string]any{"text": "mac: two"})
	text, isErr := r.call("mac", "status", map[string]any{})
	if isErr || !strings.Contains(text, "#general  2 unread, 1 to you") || strings.Contains(text, "two") {
		t.Fatalf("status = %q", text)
	}
	r.call("mac", "listen", map[string]any{})
	if text, _ = r.call("mac", "status", map[string]any{}); text != "Nothing unread." {
		t.Fatalf("status after listen = %q", text)
	}
}

func TestAgentRunStopsUntilAHumanWrites(t *testing.T) {
	r := newRig(t, "win", "mac")
	for i := 0; i < agentRunLimit; i++ {
		who := []string{"win", "mac"}[i%2]
		if text, isErr := r.call(who, "say", map[string]any{"text": "reply"}); isErr {
			t.Fatalf("line %d refused early: %q", i, text)
		}
	}
	text, isErr := r.call("win", "say", map[string]any{"text": "one more"})
	if !isErr || !strings.Contains(text, "agent lines in a row") {
		t.Fatalf("run not stopped: %q", text)
	}
	// A person writes (the page path), and the agents may go on.
	r.server.cost.humanLine("#general")
	if text, isErr = r.call("win", "say", map[string]any{"text": "thanks"}); isErr {
		t.Fatalf("after human line: %q", text)
	}
	// Quiet also ends a run.
	for i := 0; i < agentRunLimit; i++ {
		r.call("mac", "say", map[string]any{"text": "x"})
	}
	later := time.Now().Add(agentRunQuiet + time.Second)
	r.server.cost.now = func() time.Time { return later }
	if text, isErr = r.call("mac", "say", map[string]any{"text": "after a pause"}); isErr {
		t.Fatalf("after quiet: %q", text)
	}
}

func TestDailyBudget(t *testing.T) {
	r := newRig(t, "win")
	r.server.cost.dailyCap = 3
	for i := 0; i < 3; i++ {
		r.server.cost.humanLine("#general") // keep the run limit out of the way
		if _, isErr := r.call("win", "say", map[string]any{"text": "ok"}); isErr {
			t.Fatalf("line %d refused", i)
		}
	}
	if text, isErr := r.call("win", "say", map[string]any{"text": "over"}); !isErr || !strings.Contains(text, "daily budget") {
		t.Fatalf("budget not enforced: %q", text)
	}
}
