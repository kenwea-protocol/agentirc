package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// A second client with the same token is refused while the first is live,
// and the nick frees itself after sessionIdle.
func TestOneLiveSessionPerToken(t *testing.T) {
	r := newRig(t, "alice")
	post := func(sessionID, method string) (int, string) {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method,
			"params": map[string]any{"protocolVersion": "2025-06-18"}})
		req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/irc/mcp", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+r.tokens["alice"])
		if sessionID != "" {
			req.Header.Set("Mcp-Session-Id", sessionID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Mcp-Session-Id")
	}

	code, first := post("", "initialize")
	if code != http.StatusOK || first == "" {
		t.Fatalf("first initialize = %d, session %q", code, first)
	}
	if code, _ := post(first, "tools/list"); code != http.StatusOK {
		t.Errorf("own session = %d, want 200", code)
	}
	if code, _ := post("", "initialize"); code != http.StatusConflict {
		t.Errorf("second client initialize = %d, want 409", code)
	}
	if code, _ := post("", "tools/list"); code != http.StatusConflict {
		t.Errorf("request without the live session id = %d, want 409", code)
	}
	if code, _ := post("stale", "tools/list"); code != http.StatusNotFound {
		t.Errorf("request with a wrong session id = %d, want 404", code)
	}
}

func TestSessionFreesAfterIdle(t *testing.T) {
	b := newSessionBook()
	now := time.Unix(0, 0)
	b.now = func() time.Time { return now }
	first, status, _ := b.begin("alice", "")
	if status != 0 || first == "" {
		t.Fatal("first begin refused")
	}
	if _, status, _ := b.begin("alice", ""); status != http.StatusConflict {
		t.Fatalf("second begin = %d, want 409", status)
	}
	// The same client re-initializing keeps going.
	if _, status, _ := b.begin("alice", first); status != 0 {
		t.Fatalf("re-initialize with own id = %d", status)
	}
	now = now.Add(sessionIdle + time.Second)
	if _, status, _ := b.begin("alice", ""); status != 0 {
		t.Errorf("after idle = %d, want the nick free", status)
	}
}
