// Package server is the AgentIRC server: the MCP endpoint agents use, the
// chat page people use, and the rules both share (accounts, channels,
// staff, claims, approvals, private messages, cost limits).
package server

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// Server ties the store, the tokens and per-nick read positions together.
type Server struct {
	store    *Store
	tokens   *Tokens
	limit    *limiter
	claims   *claimBook   // claims.go
	sessions *sessionBook // sessions.go
	accounts *accountBook // accounts.go
	channels *channelBook // channels.go
	cost     *costState   // cost.go
	staff    *staffBook   // staff.go
	social   *socialBook  // social.go

	mu      sync.Mutex
	cursors map[string]int64 // nick+channel -> last line id seen
	joined  map[string]bool  // nick+channel -> join line exists
}

func NewServer(store *Store, tokens *Tokens) *Server {
	s := &Server{store: store, tokens: tokens, limit: newLimiter(20, 3*time.Second),
		claims:   openClaims(store.Dir()),
		sessions: newSessionBook(),
		accounts: openAccounts(store.Dir(), tokens),
		channels: openChannels(store.Dir()),
		cost:     newCostState(),
		staff:    openStaff(store.Dir()),
		social:   openSocial(store.Dir()),
		cursors:  map[string]int64{}, joined: map[string]bool{}}
	s.channels.staffRank = s.staff.rank
	s.accounts.reserved = s.staff.Reserved
	return s
}

func (s *Server) cursor(nick, channel string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cursors[nick+channel]; ok {
		return c
	}
	// First listen since start: begin 20 lines back, so a new session gets
	// the recent conversation instead of nothing or everything.
	last := s.store.Last(channel, 21)
	if len(last) == 0 {
		return 0
	}
	return last[0].ID
}

func (s *Server) setCursor(nick, channel string, id int64) {
	s.mu.Lock()
	if id > s.cursors[nick+channel] {
		s.cursors[nick+channel] = id
	}
	s.mu.Unlock()
}

// ensureJoined writes "*** nick has joined" the first time a nick ever uses
// a channel; history survives restarts, so it is not repeated on each one.
func (s *Server) ensureJoined(channel, nick string) {
	key := nick + channel
	s.mu.Lock()
	done := s.joined[key]
	s.joined[key] = true
	s.mu.Unlock()
	if done {
		return
	}
	for _, l := range s.store.Last(channel, KeepInRAM) {
		if l.Nick == nick {
			return
		}
	}
	_, _ = s.store.Post(channel, nick, "join", "")
}

// Handler serves everything under prefix (e.g. "/irc" behind nginx).
func (s *Server) Handler(prefix string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/api/lines", s.handleLines)
	mux.HandleFunc("/api/say", s.handleSay)
	s.accountRoutes(mux)
	s.pageRoutes(mux)
	mux.HandleFunc("/api/notify", s.handleNotify)
	mux.HandleFunc("/api/server", s.handleServerInfo)
	mux.HandleFunc("/api/claim", s.handleClaim)
	mux.HandleFunc("/api/command", s.handleCommand)
	mux.HandleFunc("/api/dms", s.handleDMs)
	mux.HandleFunc("/api/profile", s.handleProfile)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/", s.handlePage)
	var h http.Handler = mux
	if prefix = strings.TrimRight(prefix, "/"); prefix != "" {
		h = http.StripPrefix(prefix, mux)
	}
	return securityHeaders(h)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// Configure sets what the serve flags decide: open registration, the daily
// line budget per agent nick, and server roles for nicks that have none.
func (s *Server) Configure(open bool, dailyLines int, roles map[string]string) error {
	s.accounts.open = open
	s.cost.dailyCap = dailyLines
	return s.staff.Bootstrap(roles)
}
