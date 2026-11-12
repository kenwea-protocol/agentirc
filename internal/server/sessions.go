package server

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// One live MCP session per nick. initialize mints an Mcp-Session-Id; while
// that session has been active within sessionIdle, a second initialize with
// the same token is refused (409), and a request without the current id or
// with a stale one is refused too. A session that goes quiet for
// sessionIdle frees the nick, so a crashed client can rejoin.
//
// Why: a token copied into a second agent's configuration let that agent
// speak under someone else's nick. The nick comes from the token; this
// keeps one token to one speaker at a time.
const sessionIdle = 2 * time.Minute

type liveSession struct {
	id   string
	last time.Time
}

type sessionBook struct {
	mu     sync.Mutex
	byNick map[string]*liveSession
	now    func() time.Time
}

func newSessionBook() *sessionBook {
	return &sessionBook{byNick: map[string]*liveSession{}, now: time.Now}
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// begin starts a session for nick, or refuses when another is live. A
// client re-initializing with its own current id keeps it.
func (b *sessionBook) begin(nick, presented string) (string, int, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if cur, ok := b.byNick[nick]; ok && now.Sub(cur.last) < sessionIdle && cur.id != presented {
		return "", http.StatusConflict, "nick " + nick + " already has a live session; another client is using this token"
	}
	id := newSessionID()
	b.byNick[nick] = &liveSession{id: id, last: now}
	return id, 0, ""
}

// touch checks a non-initialize request against the live session.
func (b *sessionBook) touch(nick, presented string) (int, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur, ok := b.byNick[nick]
	if !ok || b.now().Sub(cur.last) >= sessionIdle {
		// No live session: a stateless client (curl, a script) is fine.
		if presented != "" {
			return http.StatusNotFound, "session expired; initialize again"
		}
		return 0, ""
	}
	switch presented {
	case cur.id:
		cur.last = b.now()
		return 0, ""
	case "":
		return http.StatusConflict, "nick " + nick + " has a live session; send its Mcp-Session-Id"
	default:
		return http.StatusNotFound, "session replaced or expired; initialize again"
	}
}

// end closes nick's session when the client sends DELETE with its id.
func (b *sessionBook) end(nick, presented string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cur, ok := b.byNick[nick]; ok && cur.id == presented {
		delete(b.byNick, nick)
	}
}
