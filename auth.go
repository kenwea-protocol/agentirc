package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Tokens maps the SHA-256 of a token to the nick it speaks as. The token
// itself is never stored by the server: tokens.txt holds "nick sha256hex"
// lines, so reading that file does not let anyone in.
type Tokens struct {
	byHash map[[32]byte]string
	roles  map[string]string // nick -> RoleOwner | RoleAgent | RoleGuest
}

// Roles, the optional third column of tokens.txt ("nick sha256 role").
// owner answers approvals; guest may read but never write; agent is the
// default and does everything else.
const (
	RoleOwner = "owner"
	RoleAgent = "agent"
	RoleGuest = "guest"
)

// Role returns nick's role (RoleAgent when none was given).
func (t *Tokens) Role(nick string) string {
	if r, ok := t.roles[nick]; ok {
		return r
	}
	return RoleAgent
}

var nickName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,15}$`)

// LoadTokens reads a tokens file.
func LoadTokens(path string) (*Tokens, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	t := &Tokens{byHash: map[[32]byte]string{}, roles: map[string]string{}}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		raw, err := hex.DecodeString(fieldAt(fields, 1))
		if len(fields) < 2 || len(fields) > 3 || !nickName.MatchString(fields[0]) || err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("%s:%d: want \"nick sha256hex [owner|agent|guest]\"", path, n)
		}
		if len(fields) == 3 {
			switch fields[2] {
			case RoleOwner, RoleAgent, RoleGuest:
				t.roles[fields[0]] = fields[2]
			default:
				return nil, fmt.Errorf("%s:%d: role must be owner, agent or guest", path, n)
			}
		}
		var h [32]byte
		copy(h[:], raw)
		t.byHash[h] = fields[0]
	}
	if len(t.byHash) == 0 {
		return nil, errors.New("no tokens configured")
	}
	return t, sc.Err()
}

func fieldAt(f []string, i int) string {
	if i < len(f) {
		return f[i]
	}
	return ""
}

// Nick returns who token belongs to. The lookup is by hash, and the hash
// comparison is constant-time, so a near miss tells an attacker nothing.
func (t *Tokens) Nick(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	for h, nick := range t.byHash {
		if subtle.ConstantTimeCompare(h[:], sum[:]) == 1 {
			return nick, true
		}
	}
	return "", false
}

// FromRequest accepts "Authorization: Bearer <token>" (agents) or HTTP
// Basic with the token as the password (a browser's own login prompt).
func (t *Tokens) FromRequest(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if tok, ok := strings.CutPrefix(h, "Bearer "); ok {
		return t.Nick(strings.TrimSpace(tok))
	}
	if _, pass, ok := r.BasicAuth(); ok {
		return t.Nick(pass)
	}
	return "", false
}

// NewToken returns a fresh random token and the line to add to tokens.txt.
func NewToken(nick string) (token, line string, err error) {
	if !nickName.MatchString(nick) {
		return "", "", fmt.Errorf("nick must be lower-case letters, digits, _ or -, at most 16")
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	token = "airc_" + base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(token))
	return token, nick + " " + hex.EncodeToString(sum[:]), nil
}

// limiter allows burst lines per nick, refilled at one per interval.
type limiter struct {
	mu       sync.Mutex
	burst    float64
	interval time.Duration
	left     map[string]float64
	last     map[string]time.Time
	now      func() time.Time
}

func newLimiter(burst int, interval time.Duration) *limiter {
	return &limiter{burst: float64(burst), interval: interval,
		left: map[string]float64{}, last: map[string]time.Time{}, now: time.Now}
}

func (l *limiter) allow(nick string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	left, ok := l.left[nick]
	if !ok {
		left = l.burst
	} else {
		left += float64(now.Sub(l.last[nick])) / float64(l.interval)
		if left > l.burst {
			left = l.burst
		}
	}
	l.last[nick] = now
	if left < 1 {
		l.left[nick] = left
		return false
	}
	l.left[nick] = left - 1
	return true
}
