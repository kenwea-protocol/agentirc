package main

import (
	"strings"
	"testing"
)

func TestRegisteredChannelOpsAndNotice(t *testing.T) {
	r := newRig(t, "win", "mac", "eve")
	// Anyone can talk in an unregistered channel.
	if _, isErr := r.call("eve", "say", map[string]any{"channel": "#dev", "text": "hi"}); isErr {
		t.Fatal("unregistered channel refused a line")
	}
	text, isErr := r.call("win", "register_channel", map[string]any{"channel": "#dev"})
	if isErr || !strings.Contains(text, "registers #dev") {
		t.Fatalf("register: %q", text)
	}
	if text, isErr = r.call("mac", "register_channel", map[string]any{"channel": "#dev"}); !isErr || !strings.Contains(text, "registered by win") {
		t.Fatalf("second register: %q", text)
	}
	// NOTICE and topic are for operators.
	if text, isErr = r.call("mac", "notice", map[string]any{"channel": "#dev", "text": "deploy at 5"}); !isErr {
		t.Fatalf("non-op notice allowed: %q", text)
	}
	if text, isErr = r.call("mac", "topic", map[string]any{"channel": "#dev", "text": "mine now"}); !isErr {
		t.Fatalf("non-op topic allowed: %q", text)
	}
	if text, isErr = r.call("win", "notice", map[string]any{"channel": "#dev", "text": "deploy at 5"}); isErr || !strings.Contains(text, "-win:#dev- deploy at 5") {
		t.Fatalf("op notice: %q %v", text, isErr)
	}
	// Op mac, then mac can.
	if _, isErr = r.call("win", "op", map[string]any{"channel": "#dev", "nick": "mac"}); isErr {
		t.Fatal("op failed")
	}
	if _, isErr = r.call("mac", "topic", map[string]any{"channel": "#dev", "text": "release week"}); isErr {
		t.Fatal("op could not set topic")
	}
	// The founder can hand the channel over; mac cannot take it.
	if _, isErr = r.call("mac", "transfer", map[string]any{"channel": "#dev", "nick": "mac"}); !isErr {
		t.Fatal("an op took the founder role")
	}
	// The founder cannot be deopped.
	if _, isErr = r.call("mac", "deop", map[string]any{"channel": "#dev", "nick": "win"}); !isErr {
		t.Fatal("founder deopped")
	}
	// A ban shuts eve out of reading and writing.
	if _, isErr = r.call("mac", "ban", map[string]any{"channel": "#dev", "nick": "eve"}); isErr {
		t.Fatal("ban failed")
	}
	for _, tool := range []string{"say", "listen", "history"} {
		if text, isErr = r.call("eve", tool, map[string]any{"channel": "#dev", "text": "x"}); !isErr || !strings.Contains(text, "banned") {
			t.Fatalf("banned %s: %q", tool, text)
		}
	}
	if _, isErr = r.call("win", "unban", map[string]any{"channel": "#dev", "nick": "eve"}); isErr {
		t.Fatal("unban failed")
	}
	if _, isErr = r.call("eve", "say", map[string]any{"channel": "#dev", "text": "back"}); isErr {
		t.Fatal("unbanned nick still refused")
	}
}

func TestPrivateChannelNeedsKey(t *testing.T) {
	r := newRig(t, "win", "mac")
	r.call("win", "register_channel", map[string]any{"channel": "#secret"})
	if text, isErr := r.call("win", "set_key", map[string]any{"channel": "#secret", "key": "abc"}); !isErr {
		t.Fatalf("short key accepted: %q", text)
	}
	if _, isErr := r.call("win", "set_key", map[string]any{"channel": "#secret", "key": "open sesame"}); isErr {
		t.Fatal("set_key failed")
	}
	r.call("win", "say", map[string]any{"channel": "#secret", "text": "plan B"})
	for _, tool := range []string{"listen", "history", "say"} {
		if text, isErr := r.call("mac", tool, map[string]any{"channel": "#secret", "text": "x"}); !isErr || !strings.Contains(text, "private") {
			t.Fatalf("outsider %s: %q", tool, text)
		}
	}
	// Not listed for outsiders, listed for the op.
	if text, _ := r.call("mac", "who", map[string]any{}); strings.Contains(text, "#secret") {
		t.Fatalf("private channel shown in who: %q", text)
	}
	if text, _ := r.call("win", "who", map[string]any{}); !strings.Contains(text, "#secret") {
		t.Fatalf("op does not see own channel: %q", text)
	}
	if text, isErr := r.call("mac", "join", map[string]any{"channel": "#secret", "key": "wrong key!"}); !isErr {
		t.Fatalf("wrong key joined: %q", text)
	}
	if _, isErr := r.call("mac", "join", map[string]any{"channel": "#secret", "key": "open sesame"}); isErr {
		t.Fatal("right key refused")
	}
	if text, isErr := r.call("mac", "history", map[string]any{"channel": "#secret"}); isErr || !strings.Contains(text, "plan B") {
		t.Fatalf("member cannot read: %q", text)
	}
	// A new key drops old members.
	r.call("win", "set_key", map[string]any{"channel": "#secret", "key": "new key 2026"})
	if _, isErr := r.call("mac", "history", map[string]any{"channel": "#secret"}); !isErr {
		t.Fatal("member kept access after key change")
	}
	// Nothing about the key is in the room.
	text, _ := r.call("win", "history", map[string]any{"channel": "#secret"})
	if strings.Contains(text, "open sesame") || strings.Contains(text, "new key 2026") {
		t.Fatal("key leaked into the channel")
	}
}
