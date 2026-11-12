package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestPrivateMessagesStayBetweenTwo(t *testing.T) {
	r := newRig(t, "win", "mac", "eve")
	if text, isErr := r.call("win", "msg", map[string]any{"nick": "mac", "text": "only for you"}); isErr || !strings.Contains(text, "(to mac)") {
		t.Fatalf("msg: %q", text)
	}
	if text, _ := r.call("mac", "dm_history", map[string]any{"nick": "win"}); !strings.Contains(text, "only for you") {
		t.Fatalf("mac cannot read: %q", text)
	}
	if text, _ := r.call("eve", "dm_history", map[string]any{"nick": "win"}); strings.Contains(text, "only for you") {
		t.Fatalf("eve read someone else's conversation: %q", text)
	}
	// Not a channel: not in who, not in list_channels, not readable as one.
	if text, _ := r.call("eve", "who", map[string]any{}); strings.Contains(text, "=mac=win") {
		t.Fatalf("conversation listed as a channel: %q", text)
	}
	if text, _ := r.call("eve", "list_channels", map[string]any{}); strings.Contains(text, "only for you") || strings.Contains(text, "=") {
		t.Fatalf("list_channels: %q", text)
	}
	if text, _ := r.call("mac", "dms", map[string]any{}); !strings.Contains(text, "win") {
		t.Fatalf("dms: %q", text)
	}
	// A new one reaches the free hook (the first was read already).
	r.call("win", "msg", map[string]any{"nick": "mac", "text": "second note"})
	_, body := r.notify("mac", "")
	if !strings.Contains(body, "second note") || strings.Contains(body, "only for you") {
		t.Fatalf("notify missed the private message: %q", body)
	}
	if text, isErr := r.call("win", "msg", map[string]any{"nick": "nobody", "text": "x"}); !isErr || !strings.Contains(text, "no nick") {
		t.Fatalf("message to an unknown nick: %q", text)
	}
}

func TestIgnoreHidesLinesAndBlocksMessages(t *testing.T) {
	r := newRig(t, "win", "eve")
	r.call("eve", "say", map[string]any{"text": "spam spam"})
	r.call("win", "say", map[string]any{"text": "real talk"})
	if text, isErr := r.call("win", "ignore", map[string]any{"nick": "eve"}); isErr {
		t.Fatalf("ignore: %q", text)
	}
	text, _ := r.call("win", "history", map[string]any{})
	if strings.Contains(text, "spam spam") || !strings.Contains(text, "real talk") {
		t.Fatalf("history with ignore: %q", text)
	}
	if text, isErr := r.call("eve", "msg", map[string]any{"nick": "win", "text": "hello?"}); !isErr || !strings.Contains(text, "does not take private messages") {
		t.Fatalf("ignored nick could message: %q", text)
	}
	r.call("win", "ignore", map[string]any{"nick": "eve", "on": false})
	if text, _ := r.call("win", "history", map[string]any{}); !strings.Contains(text, "spam spam") {
		t.Fatalf("unignore did not show lines: %q", text)
	}
}

func TestProfileShowsOnlyWhatIsPublic(t *testing.T) {
	r := newRig(t, "win", "eve")
	if err := r.server.social.SaveProfile("win",
		map[string]string{"name": "Win Session", "location": "Istanbul", "website": "kenwea.com"},
		map[string]bool{"name": true, "website": true}, "all"); err != nil {
		t.Fatal(err)
	}
	text, _ := r.call("eve", "profile", map[string]any{"nick": "win"})
	if !strings.Contains(text, "Win Session") || !strings.Contains(text, "https://kenwea.com") || strings.Contains(text, "Istanbul") {
		t.Fatalf("profile: %q", text)
	}
	// DMs off: nobody but staff may write.
	r.server.social.SaveProfile("win", nil, nil, "nobody")
	if _, isErr := r.call("eve", "msg", map[string]any{"nick": "win", "text": "hi"}); !isErr {
		t.Fatal("message sent to a nick that takes none")
	}
}

func TestPageReadsOnlyItsOwnConversation(t *testing.T) {
	r := newRig(t, "ahmet", "mac", "eve")
	r.call("mac", "msg", map[string]any{"nick": "ahmet", "text": "private note"})
	w := newWebClient(t, r)
	w.do("POST", "/api/login-token", map[string]string{"token": r.tokens["eve"]}, nil)
	code, out := w.do("GET", "/api/lines?dm=mac", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("dm lines = %d", code)
	}
	if lines, _ := out["lines"].([]any); len(lines) != 0 {
		t.Fatalf("eve saw another conversation: %v", out)
	}
	w2 := newWebClient(t, r)
	w2.do("POST", "/api/login-token", map[string]string{"token": r.tokens["ahmet"]}, nil)
	_, out = w2.do("GET", "/api/lines?dm=mac", nil, nil)
	if lines, _ := out["lines"].([]any); len(lines) != 1 {
		t.Fatalf("ahmet's conversation: %v", out)
	}
}
