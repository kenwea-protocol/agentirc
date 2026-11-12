// Command agentirc is a small IRC-style chat room for AI coding sessions
// on different machines and their human owner. Agents join over MCP
// (mcp.go); the owner watches and talks in a mIRC-looking page (web.go).
//
//	agentirc token <nick> -tokens tokens.txt -out <file>   add a nick
//	agentirc serve -addr 127.0.0.1:8770 -data ./data -tokens tokens.txt
//
// A new nick's token is written to -out (mode 0600) and never printed, so
// it does not end up in a terminal log or a chat transcript.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	// Zone data inside the binary: the container image has none, and lines
	// are timestamped in the owner's zone (TZ, e.g. UTC).
	_ "time/tzdata"
)

// Server ties the store, the tokens and per-nick read positions together.
type Server struct {
	store    *Store
	tokens   *Tokens
	limit    *limiter
	claims   *claimBook   // claims.go
	sessions *sessionBook // sessions.go
	accounts *accountBook // accounts.go

	mu      sync.Mutex
	cursors map[string]int64 // nick+channel -> last line id seen
	joined  map[string]bool  // nick+channel -> join line exists
}

func NewServer(store *Store, tokens *Tokens) *Server {
	return &Server{store: store, tokens: tokens, limit: newLimiter(20, 3*time.Second),
		claims:   openClaims(store.dir),
		sessions: newSessionBook(),
		accounts: openAccounts(store.dir, tokens),
		cursors:  map[string]int64{}, joined: map[string]bool{}}
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
	for _, l := range s.store.Last(channel, keepInRAM) {
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

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agentirc serve|token ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "token":
		err = addToken(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentirc:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8770", "listen address (keep it on loopback behind a TLS proxy)")
	data := fs.String("data", "./data", "directory for the chat log")
	tokensPath := fs.String("tokens", "tokens.txt", "file of \"nick sha256\" lines")
	prefix := fs.String("prefix", "", "URL path prefix the proxy forwards, e.g. /irc")
	open := fs.Bool("open", false, "let anyone register a nick with a password (a public server); the tokens file is then optional")
	_ = fs.Parse(args)
	// Git Bash on Windows rewrites "/irc" into "C:/Program Files/Git/irc";
	// a prefix like that would 404 everything without saying why.
	if *prefix != "" && !strings.HasPrefix(*prefix, "/") {
		return fmt.Errorf("-prefix %q must start with /; in Git Bash set MSYS_NO_PATHCONV=1", *prefix)
	}

	tokens, err := LoadTokens(*tokensPath)
	if err != nil {
		if !*open || !errors.Is(err, os.ErrNotExist) {
			return err
		}
		tokens = EmptyTokens()
	}
	store, err := OpenStore(*data)
	if err != nil {
		return err
	}
	server := NewServer(store, tokens)
	server.accounts.open = *open
	srv := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(*prefix),
		ReadHeaderTimeout: 10 * time.Second,
		// Longer than the 50 s a listen may wait.
		WriteTimeout: 70 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	log.Printf("agentirc listening on %s (prefix %q, open %v)", *addr, *prefix, *open)
	return srv.ListenAndServe()
}

func addToken(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: agentirc token <nick> -tokens tokens.txt -out <file>")
	}
	nick := args[0]
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	tokensPath := fs.String("tokens", "tokens.txt", "tokens file to append to")
	out := fs.String("out", "", "file to write the new token to (required; it is never printed)")
	_ = fs.Parse(args[1:])
	if *out == "" {
		return errors.New("-out is required: the token is written to a file, never printed")
	}
	token, line, err := NewToken(nick)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(*tokensPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		return err
	}
	fmt.Printf("added nick %q; its token is in %s\n", nick, *out)
	return nil
}
