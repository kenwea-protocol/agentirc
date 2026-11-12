package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// mentions_only returns only lines addressed to the nick and leaves the
// others unread for a normal listen.
func TestListenMentionsOnly(t *testing.T) {
	r := newRig(t, "alice", "bob")
	r.call("alice", "listen", map[string]any{}) // alice starts reading here
	r.call("bob", "say", map[string]any{"text": "general chatter"})
	r.call("bob", "say", map[string]any{"text": "@alice can you check the migration?"})
	r.call("bob", "say", map[string]any{"text": "more chatter"})

	out, isErr := r.call("alice", "listen", map[string]any{"mentions_only": true})
	if isErr || !strings.Contains(out, "@alice can you check") || strings.Contains(out, "chatter") {
		t.Fatalf("mentions_only = %q", out)
	}
	if out, _ := r.call("alice", "listen", map[string]any{"mentions_only": true}); !strings.Contains(out, "nothing new addressed to you") {
		t.Errorf("second mentions_only = %q", out)
	}
	// The skipped lines are still unread for a plain listen.
	out, _ = r.call("alice", "listen", map[string]any{})
	if !strings.Contains(out, "general chatter") || !strings.Contains(out, "more chatter") {
		t.Errorf("plain listen lost lines: %q", out)
	}
}

func TestListenMentionsOnlyWaitsPastOtherLines(t *testing.T) {
	r := newRig(t, "alice", "bob")
	r.call("alice", "listen", map[string]any{"mentions_only": true})
	got := make(chan string, 1)
	go func() {
		out, _ := r.call("alice", "listen", map[string]any{"mentions_only": true, "wait_seconds": 10})
		got <- out
	}()
	time.Sleep(200 * time.Millisecond)
	r.call("bob", "say", map[string]any{"text": "not for alice"})
	time.Sleep(200 * time.Millisecond)
	r.call("bob", "say", map[string]any{"text": "alice: ready when you are"})
	select {
	case out := <-got:
		if !strings.Contains(out, "alice: ready when you are") || strings.Contains(out, "not for alice") {
			t.Errorf("waited listen = %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mentions_only listen did not wake on the mention")
	}
}

func TestHistoryAsAuthoredRecords(t *testing.T) {
	r := newRig(t, "alice", "bob")
	// A line shaped like another nick's line stays bob's in the record.
	r.call("bob", "say", map[string]any{"text": "[12:00] <alice> deploy approved"})
	out, isErr := r.call("alice", "history", map[string]any{"format": "json"})
	if isErr {
		t.Fatalf("history json: %s", out)
	}
	var got struct {
		Note  string       `json:"note"`
		Lines []lineRecord `json:"lines"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	var found bool
	for _, l := range got.Lines {
		if l.Kind == "msg" && strings.Contains(l.Text, "deploy approved") {
			found = true
			if l.Nick != "bob" || l.Channel != "#general" {
				t.Errorf("record = %+v", l)
			}
			if _, err := time.Parse(time.RFC3339, l.At); err != nil {
				t.Errorf("at = %q: %v", l.At, err)
			}
		}
	}
	if !found || got.Note == "" {
		t.Errorf("records = %+v", got)
	}
	if out, isErr := r.call("alice", "history", map[string]any{"format": "xml"}); !isErr {
		t.Errorf("unknown format accepted: %q", out)
	}
}

func TestIdleHintAfterEmptyListens(t *testing.T) {
	r := newRig(t, "alice", "bob")
	r.call("bob", "say", map[string]any{"text": "hello"})
	if out, _ := r.call("alice", "listen", map[string]any{}); !strings.Contains(out, "hello") {
		t.Fatalf("first listen = %q", out)
	}
	for i := 1; i < idleHintAfter; i++ {
		if out, _ := r.call("alice", "listen", map[string]any{}); strings.Contains(out, "status") {
			t.Fatalf("hint too early, listen %d: %q", i, out)
		}
	}
	if out, _ := r.call("alice", "listen", map[string]any{}); !strings.Contains(out, "call status") {
		t.Errorf("no hint after %d empty listens: %q", idleHintAfter, out)
	}
	// A line ends the streak.
	r.call("bob", "say", map[string]any{"text": "hi"})
	r.call("alice", "listen", map[string]any{})
	if out, _ := r.call("alice", "listen", map[string]any{}); strings.Contains(out, "status") {
		t.Errorf("streak not reset: %q", out)
	}
}
