package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Accounts (2026-11-12): an open server lets anyone register a nick with a
// password. The nick is then theirs: nobody else can speak as it. A person
// signs in on the page with the password (a cookie session); an agent gets
// its own token, made on the page and shown once, so the password never goes
// into an agent's config.
//
// Stored in data/accounts.json: the password as PBKDF2-SHA256, the agent
// tokens and the page sessions as SHA-256 only. Reading the file lets nobody in.

// pbkdf2Iter is a variable only so tests can lower it.
var pbkdf2Iter = 600_000

const (
	minPassword       = 10
	maxPassword       = 200
	maxTokensPerNick  = 10
	maxTokenLabel     = 40
	webSessionTTL     = 30 * 24 * time.Hour
	sessionCookieName = "agentirc_session"
)

// Nicks nobody may register: they read as the server or its staff.
var reservedNicks = map[string]bool{
	"admin": true, "administrator": true, "root": true, "system": true, "server": true,
	"owner": true, "operator": true, "op": true, "mod": true, "moderator": true,
	"nickserv": true, "chanserv": true, "memoserv": true, "operserv": true, "irc": true,
	"agentirc": true, "kenwea": true, "kenwea-protocol": true, "support": true, "help": true,
	"anthropic": true, "claude": true, "openai": true, "everyone": true, "here": true,
}

type agentToken struct {
	Hash    string    `json:"hash"` // sha256 hex
	Label   string    `json:"label"`
	Created time.Time `json:"created"`
}

type account struct {
	Salt     string       `json:"salt"`
	Hash     string       `json:"hash"`
	Created  time.Time    `json:"created"`
	Tokens   []agentToken `json:"tokens,omitempty"`
	Sessions []webSession `json:"sessions,omitempty"`
}

type webSession struct {
	Hash    string    `json:"hash"`
	Expires time.Time `json:"expires"`
}

type accountBook struct {
	mu       sync.Mutex
	file     string
	byNick   map[string]*account
	tokens   *Tokens
	open     bool // registration allowed
	now      func() time.Time
	reserved func(nick string) bool // staff roles and invites (server.go)
	regLimit *limiter               // per client address
	logLimit *limiter               // failed sign-ins per nick and per address
}

var (
	errNickTaken     = errors.New("that nick is registered already")
	errBadLogin      = errors.New("wrong nick or password")
	errNotOpen       = errors.New("this server does not take registrations")
	errSlowDown      = errors.New("too many attempts; wait a while and try again")
	errNotSignedIn   = errors.New("sign in first")
	errTooManyTokens = errors.New("too many agent tokens for this nick; revoke one first")
)

func openAccounts(dir string, tokens *Tokens) *accountBook {
	b := &accountBook{
		file:     filepath.Join(dir, "accounts.json"),
		byNick:   map[string]*account{},
		tokens:   tokens,
		now:      time.Now,
		regLimit: newLimiter(5, 12*time.Minute), // 5 at once, then one per 12 minutes
		logLimit: newLimiter(10, time.Minute),
	}
	if raw, err := os.ReadFile(b.file); err == nil {
		_ = json.Unmarshal(raw, &b.byNick)
	}
	for nick, a := range b.byNick {
		for _, t := range a.Tokens {
			if sum, ok := hexSum(t.Hash); ok {
				tokens.add(nick, sum, "")
			}
		}
	}
	return b
}

func hexSum(h string) ([32]byte, bool) {
	var sum [32]byte
	raw, err := hex.DecodeString(h)
	if err != nil || len(raw) != 32 {
		return sum, false
	}
	copy(sum[:], raw)
	return sum, true
}

func (b *accountBook) saveLocked() error {
	raw, err := json.MarshalIndent(b.byNick, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.file)
}

func hashPassword(password string, salt []byte) (string, error) {
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(key), nil
}

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// Register creates an account. The nick must be free in the tokens file and
// among accounts, and not reserved.
func (b *accountBook) Register(nick, password, client string) error {
	if !b.open {
		return errNotOpen
	}
	nick = strings.ToLower(strings.TrimSpace(nick))
	if !nickName.MatchString(nick) {
		return fmt.Errorf("%w: a nick is a lower-case letter, then letters, digits, _ or -, at most 16", ErrBadInput)
	}
	if reservedNicks[nick] {
		return fmt.Errorf("%w: %q is reserved", ErrBadInput, nick)
	}
	if n := len([]rune(password)); n < minPassword || n > maxPassword {
		return fmt.Errorf("%w: the password needs %d to %d characters", ErrBadInput, minPassword, maxPassword)
	}
	if b.reserved != nil && b.reserved(nick) {
		return fmt.Errorf("%w: %q is reserved for a staff invite", errNickTaken, nick)
	}
	if !b.regLimit.allow("reg " + client) {
		return errSlowDown
	}
	return b.create(nick, password)
}

// create stores a new account; callers have checked the nick and password.
func (b *accountBook) create(nick, password string) error {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	hash, err := hashPassword(password, salt)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.byNick[nick]; ok || b.tokens.has(nick) {
		return errNickTaken
	}
	b.byNick[nick] = &account{Salt: hex.EncodeToString(salt), Hash: hash, Created: b.now().UTC()}
	return b.saveLocked()
}

// Login checks the password and returns a new page session id.
func (b *accountBook) Login(nick, password, client string) (string, error) {
	nick = strings.ToLower(strings.TrimSpace(nick))
	if !b.logLimit.allow("ip "+client) || !b.logLimit.allow("nick "+nick) {
		return "", errSlowDown
	}
	b.mu.Lock()
	a, ok := b.byNick[nick]
	var salt []byte
	var want string
	if ok {
		salt, _ = hex.DecodeString(a.Salt)
		want = a.Hash
	}
	b.mu.Unlock()
	if ok && want == "" {
		ok = false // a token nick's page-session holder has no password
	}
	if !ok {
		// Spend the same time as a real check, so timing does not tell
		// which nicks exist.
		salt, want = make([]byte, 16), strings.Repeat("0", 64)
	}
	got, err := hashPassword(password, salt)
	if err != nil {
		return "", err
	}
	if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return "", errBadLogin
	}
	id := randomHex(32)
	sum := sha256.Sum256([]byte(id))
	b.mu.Lock()
	defer b.mu.Unlock()
	a = b.byNick[nick]
	now := b.now()
	kept := a.Sessions[:0]
	for _, s := range a.Sessions {
		if s.Expires.After(now) {
			kept = append(kept, s)
		}
	}
	a.Sessions = append(kept, webSession{Hash: hex.EncodeToString(sum[:]), Expires: now.Add(webSessionTTL).UTC()})
	if len(a.Sessions) > 20 {
		a.Sessions = a.Sessions[len(a.Sessions)-20:]
	}
	return id, b.saveLocked()
}

// Count is the number of accounts with a password.
func (b *accountBook) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, a := range b.byNick {
		if a.Hash != "" {
			n++
		}
	}
	return n
}

// Exists reports whether nick has an account.
func (b *accountBook) Exists(nick string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.byNick[nick]
	return ok
}

// SessionNick returns the nick a page session id belongs to.
func (b *accountBook) SessionNick(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(id))
	want := hex.EncodeToString(sum[:])
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for nick, a := range b.byNick {
		for _, s := range a.Sessions {
			if s.Expires.After(now) && subtle.ConstantTimeCompare([]byte(s.Hash), []byte(want)) == 1 {
				return nick, true
			}
		}
	}
	return "", false
}

// Logout ends one page session.
func (b *accountBook) Logout(id string) {
	sum := sha256.Sum256([]byte(id))
	want := hex.EncodeToString(sum[:])
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, a := range b.byNick {
		for i, s := range a.Sessions {
			if s.Hash == want {
				a.Sessions = append(a.Sessions[:i], a.Sessions[i+1:]...)
				_ = b.saveLocked()
				return
			}
		}
	}
}

// FromCookie returns the signed-in nick of a page request.
func (b *accountBook) FromCookie(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "", false
	}
	return b.SessionNick(c.Value)
}

// NewAgentToken makes a token for nick's agents; the token is returned once
// and only its hash is kept.
func (b *accountBook) NewAgentToken(nick, label string) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		label = "agent"
	}
	if len([]rune(label)) > maxTokenLabel {
		return "", fmt.Errorf("%w: label is longer than %d characters", ErrBadInput, maxTokenLabel)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	a, ok := b.byNick[nick]
	if !ok {
		return "", errNotSignedIn
	}
	if len(a.Tokens) >= maxTokensPerNick {
		return "", errTooManyTokens
	}
	token, _, err := NewToken(nick)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(token))
	a.Tokens = append(a.Tokens, agentToken{Hash: hex.EncodeToString(sum[:]), Label: label, Created: b.now().UTC()})
	if err := b.saveLocked(); err != nil {
		return "", err
	}
	b.tokens.add(nick, sum, "")
	return token, nil
}

type tokenInfo struct {
	ID      string    `json:"id"` // first 12 hex characters of the hash
	Label   string    `json:"label"`
	Created time.Time `json:"created"`
}

// AgentTokens lists nick's tokens without anything that lets one in.
func (b *accountBook) AgentTokens(nick string) []tokenInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []tokenInfo
	if a, ok := b.byNick[nick]; ok {
		for _, t := range a.Tokens {
			out = append(out, tokenInfo{ID: t.Hash[:12], Label: t.Label, Created: t.Created})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// RevokeAgentToken removes one of nick's tokens by its id.
func (b *accountBook) RevokeAgentToken(nick, id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	a, ok := b.byNick[nick]
	if !ok || len(id) < 12 {
		return false
	}
	for i, t := range a.Tokens {
		if strings.HasPrefix(t.Hash, strings.ToLower(id)) {
			if sum, ok := hexSum(t.Hash); ok {
				b.tokens.remove(sum)
			}
			a.Tokens = append(a.Tokens[:i], a.Tokens[i+1:]...)
			_ = b.saveLocked()
			return true
		}
	}
	return false
}

// clientAddr is the caller's address: X-Real-IP when the request came from a
// proxy on this host, the socket address otherwise.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
			return real
		}
	}
	return host
}

// sameOrigin refuses a cross-site form post: JSON only, and an Origin, when
// sent, must be this host.
func sameOrigin(r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	return origin == "https://"+r.Host || origin == "http://"+r.Host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func errorStatus(err error) int {
	switch {
	case errors.Is(err, ErrBadInput):
		return http.StatusBadRequest
	case errors.Is(err, errNickTaken):
		return http.StatusConflict
	case errors.Is(err, errBadLogin), errors.Is(err, errNotSignedIn):
		return http.StatusUnauthorized
	case errors.Is(err, errNotOpen):
		return http.StatusForbidden
	case errors.Is(err, errSlowDown):
		return http.StatusTooManyRequests
	case errors.Is(err, errTooManyTokens):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, id string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: id, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteStrictMode,
	})
}

// accountRoutes adds the account API to mux.
func (s *Server) accountRoutes(mux *http.ServeMux) {
	readCreds := func(w http.ResponseWriter, r *http.Request) (string, string, bool) {
		if r.Method != http.MethodPost || !sameOrigin(r) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "POST JSON from this site only"})
			return "", "", false
		}
		var in struct{ Nick, Password string }
		if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in) != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad JSON"})
			return "", "", false
		}
		return in.Nick, in.Password, true
	}
	signIn := func(w http.ResponseWriter, r *http.Request, nick, password string, status int) {
		id, err := s.accounts.Login(nick, password, clientAddr(r))
		if err != nil {
			writeJSON(w, errorStatus(err), map[string]string{"error": err.Error()})
			return
		}
		setSessionCookie(w, r, id, int(webSessionTTL/time.Second))
		writeJSON(w, status, map[string]string{"nick": strings.ToLower(strings.TrimSpace(nick))})
	}
	mux.HandleFunc("/api/register", func(w http.ResponseWriter, r *http.Request) {
		nick, password, ok := readCreds(w, r)
		if !ok {
			return
		}
		if err := s.accounts.Register(nick, password, clientAddr(r)); err != nil {
			writeJSON(w, errorStatus(err), map[string]string{"error": err.Error()})
			return
		}
		signIn(w, r, nick, password, http.StatusCreated)
	})
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		nick, password, ok := readCreds(w, r)
		if ok {
			signIn(w, r, nick, password, http.StatusOK)
		}
	})
	mux.HandleFunc("/api/logout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !sameOrigin(r) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "POST JSON from this site only"})
			return
		}
		if c, err := r.Cookie(sessionCookieName); err == nil {
			s.accounts.Logout(c.Value)
		}
		setSessionCookie(w, r, "", -1)
		writeJSON(w, http.StatusOK, map[string]string{})
	})
	mux.HandleFunc("/api/tokens", func(w http.ResponseWriter, r *http.Request) {
		nick, ok := s.accounts.FromCookie(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": errNotSignedIn.Error()})
			return
		}
		if r.Method != http.MethodGet && !sameOrigin(r) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON from this site only"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{"nick": nick, "tokens": s.accounts.AgentTokens(nick)})
		case http.MethodPost:
			var in struct{ Label string }
			_ = json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in)
			token, err := s.accounts.NewAgentToken(nick, in.Label)
			if err != nil {
				writeJSON(w, errorStatus(err), map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]string{"token": token,
				"note": "shown once; put it in your agent's MCP config as Authorization: Bearer <token>"})
		case http.MethodDelete:
			var in struct{ ID string }
			_ = json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in)
			if !s.accounts.RevokeAgentToken(nick, in.ID) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such token"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET, POST or DELETE"})
		}
	})
}

//go:embed signin.html
var signinHTML []byte

//go:embed account.html
var accountHTML []byte

const pageCSP = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'"

func servePage(w http.ResponseWriter, page []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", pageCSP)
	_, _ = w.Write(page)
}

// TokenLogin starts a page session for a nick that signs in with a token
// (nicks from the tokens file have no password). It keeps the session under
// a password-less entry, which Login never accepts.
func (b *accountBook) TokenLogin(token, client string) (string, string, error) {
	if !b.logLimit.allow("ip " + client) {
		return "", "", errSlowDown
	}
	nick, ok := b.tokens.Nick(token)
	if !ok {
		return "", "", errBadLogin
	}
	id := randomHex(32)
	sum := sha256.Sum256([]byte(id))
	b.mu.Lock()
	defer b.mu.Unlock()
	a, ok := b.byNick[nick]
	if !ok {
		a = &account{Created: b.now().UTC()}
		b.byNick[nick] = a
	}
	now := b.now()
	kept := a.Sessions[:0]
	for _, s := range a.Sessions {
		if s.Expires.After(now) {
			kept = append(kept, s)
		}
	}
	a.Sessions = append(kept, webSession{Hash: hex.EncodeToString(sum[:]), Expires: now.Add(webSessionTTL).UTC()})
	return id, nick, b.saveLocked()
}

// pageRoutes adds /account and /api/login-token.
func (s *Server) pageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/account", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.accounts.FromCookie(r); !ok {
			http.Redirect(w, r, "./", http.StatusSeeOther)
			return
		}
		servePage(w, accountHTML)
	})
	mux.HandleFunc("/api/login-token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !sameOrigin(r) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "POST JSON from this site only"})
			return
		}
		var in struct{ Token string }
		if json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in) != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad JSON"})
			return
		}
		id, nick, err := s.accounts.TokenLogin(strings.TrimSpace(in.Token), clientAddr(r))
		if err != nil {
			msg := err.Error()
			if errors.Is(err, errBadLogin) {
				msg = "that token is not valid here"
			}
			writeJSON(w, errorStatus(err), map[string]string{"error": msg})
			return
		}
		setSessionCookie(w, r, id, int(webSessionTTL/time.Second))
		writeJSON(w, http.StatusOK, map[string]string{"nick": nick})
	})
}
