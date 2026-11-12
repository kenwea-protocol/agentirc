package server

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func newBook(t *testing.T) (*claimBook, *time.Time) {
	t.Helper()
	b := openClaims(t.TempDir())
	now := time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return now }
	return b, &now
}

func TestNormalizeClaimPath(t *testing.T) {
	cases := map[string]string{
		"server/auth":             "server/auth",
		"./server/auth/":          "server/auth",
		`mobile\lib\setup`:        "mobile/lib/setup",
		"mobile/lib/l10n/*.arb":   "mobile/lib/l10n",
		"mobile/lib/features/**":  "mobile/lib/features",
		"/README.md":              "README.md",
		".":                       ".",
		"*":                       ".",
		"server//auth/./store.go": "server/auth/store.go",
	}
	for in, want := range cases {
		got, err := normalizeClaimPath(in)
		if err != nil || got != want {
			t.Errorf("normalizeClaimPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "  ", "../other", "a/../../b"} {
		if _, err := normalizeClaimPath(bad); err == nil {
			t.Errorf("normalizeClaimPath(%q) accepted", bad)
		}
	}
}

func TestClaimsOverlap(t *testing.T) {
	yes := [][2]string{{"a/b", "a/b"}, {"a", "a/b/c"}, {"a/b/c", "a"}, {".", "x"}, {"Server/Auth", "server/auth/x.go"}}
	no := [][2]string{{"a/b", "a/bc"}, {"a/b", "a/c"}, {"server", "serverless"}}
	for _, p := range yes {
		if !claimsOverlap(p[0], p[1]) {
			t.Errorf("%q and %q should overlap", p[0], p[1])
		}
	}
	for _, p := range no {
		if claimsOverlap(p[0], p[1]) {
			t.Errorf("%q and %q should not overlap", p[0], p[1])
		}
	}
}

func TestClaimRefusesOverlapAndNamesHolder(t *testing.T) {
	b, _ := newBook(t)
	if _, err := b.Claim("win", []string{"server/auth"}, 0, "token binding"); err != nil {
		t.Fatal(err)
	}
	_, err := b.Claim("mac", []string{"README.md", "server/auth/store.go"}, 0, "")
	if !errors.Is(err, errClaimRefused) {
		t.Fatalf("want refusal, got %v", err)
	}
	for _, want := range []string{"held by win", "server/auth", "token binding"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	// All or nothing: README.md was free but must not have been claimed.
	for _, c := range b.List() {
		if c.Nick == "mac" {
			t.Fatalf("refused claim left %v behind", c)
		}
	}
	// A parent directory is refused too.
	if _, err := b.Claim("mac", []string{"server"}, 0, ""); !errors.Is(err, errClaimRefused) {
		t.Fatalf("parent dir: want refusal, got %v", err)
	}
	// A sibling is fine.
	if _, err := b.Claim("mac", []string{"server/web"}, 0, ""); err != nil {
		t.Fatalf("sibling: %v", err)
	}
}

func TestClaimRenewsOwnAndExpires(t *testing.T) {
	b, now := newBook(t)
	if _, err := b.Claim("win", []string{"a"}, 10*time.Minute, "first"); err != nil {
		t.Fatal(err)
	}
	// Own overlapping and repeated claims are fine; a repeat renews.
	if _, err := b.Claim("win", []string{"a", "a/b"}, 20*time.Minute, "second"); err != nil {
		t.Fatal(err)
	}
	list := b.List()
	if len(list) != 2 || list[0].Note != "second" || !list[0].Expires.Equal(now.Add(20*time.Minute)) {
		t.Fatalf("renew: %+v", list)
	}
	*now = now.Add(21 * time.Minute)
	if got := b.List(); len(got) != 0 {
		t.Fatalf("expired claims still listed: %+v", got)
	}
	if _, err := b.Claim("mac", []string{"a"}, 0, ""); err != nil {
		t.Fatalf("claim after expiry: %v", err)
	}
}

func TestClaimTTLBounds(t *testing.T) {
	b, now := newBook(t)
	got, err := b.Claim("win", []string{"x"}, 24*time.Hour, "")
	if err != nil || !got[0].Expires.Equal(now.Add(maxClaimTTL)) {
		t.Fatalf("ttl cap: %+v %v", got, err)
	}
	got, _ = b.Claim("win", []string{"y"}, 0, "")
	if !got[0].Expires.Equal(now.Add(defaultClaimTTL)) {
		t.Fatalf("default ttl: %+v", got)
	}
}

func TestReleaseOnlyOwn(t *testing.T) {
	b, _ := newBook(t)
	b.Claim("win", []string{"a", "b"}, 0, "")
	b.Claim("mac", []string{"c"}, 0, "")
	if gone, _ := b.Release("mac", []string{"a"}); len(gone) != 0 {
		t.Fatalf("mac released win's claim: %v", gone)
	}
	if gone, _ := b.Release("win", []string{"./a/"}); len(gone) != 1 || gone[0] != "a" {
		t.Fatalf("release a: %v", gone)
	}
	if gone, _ := b.Release("win", nil); len(gone) != 1 || gone[0] != "b" {
		t.Fatalf("release all: %v", gone)
	}
	if l := b.List(); len(l) != 1 || l[0].Nick != "mac" {
		t.Fatalf("left: %+v", l)
	}
}

func TestClaimsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	b := openClaims(dir)
	if _, err := b.Claim("win", []string{"server"}, time.Hour, "kept"); err != nil {
		t.Fatal(err)
	}
	again := openClaims(dir)
	if _, err := again.Claim("mac", []string{"server/x"}, 0, ""); !errors.Is(err, errClaimRefused) {
		t.Fatalf("claim lost on restart: %v", err)
	}
}

// Two sessions racing for the same path: exactly one gets a yes.
func TestClaimRaceHasOneWinner(t *testing.T) {
	for round := 0; round < 50; round++ {
		b, _ := newBook(t)
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for _, n := range []string{"win", "mac", "kenwea", "x"} {
			wg.Add(1)
			go func(n string) {
				defer wg.Done()
				if _, err := b.Claim(n, []string{"shared/file.go"}, 0, ""); err == nil {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}(n)
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("round %d: %d winners", round, wins)
		}
	}
}

func TestClaimToolsOverMCP(t *testing.T) {
	r := newRig(t, "win", "mac")
	_, out := r.rpc("win", "tools/list", map[string]any{})
	names := ""
	for _, tl := range out["result"].(map[string]any)["tools"].([]any) {
		names += tl.(map[string]any)["name"].(string) + " "
	}
	for _, want := range []string{"claim", "release", "claims", "say"} {
		if !strings.Contains(names, want+" ") {
			t.Fatalf("tools/list lacks %s: %s", want, names)
		}
	}
	text, isErr := r.call("win", "claim", map[string]any{"paths": []string{"server/auth"}, "note": "approvals"})
	if isErr || !strings.Contains(text, "Claimed server/auth") {
		t.Fatalf("claim: %q %v", text, isErr)
	}
	text, isErr = r.call("mac", "claim", map[string]any{"paths": []string{"server"}})
	if !isErr || !strings.Contains(text, "held by win") || !strings.Contains(text, "approvals") {
		t.Fatalf("overlap over MCP: %q %v", text, isErr)
	}
	text, _ = r.call("mac", "claims", map[string]any{})
	if !strings.Contains(text, "server/auth  held by win") {
		t.Fatalf("claims: %q", text)
	}
	// The room sees it as a /me line.
	text, _ = r.call("mac", "history", map[string]any{})
	if !strings.Contains(text, "* win claims server/auth until") {
		t.Fatalf("no claim line in history: %q", text)
	}
	if text, isErr = r.call("win", "release", map[string]any{}); isErr || !strings.Contains(text, "Released server/auth") {
		t.Fatalf("release: %q %v", text, isErr)
	}
	if text, isErr = r.call("mac", "claim", map[string]any{"paths": []string{"server"}}); isErr {
		t.Fatalf("claim after release: %q", text)
	}
}
