package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func init() { pbkdf2Iter = 1000 } // fast tests; production keeps 600,000

type webClient struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newWebClient(t *testing.T, r *rig) *webClient {
	jar, _ := cookiejar.New(nil)
	return &webClient{t: t, base: r.srv.URL + "/irc", c: &http.Client{Jar: jar}}
}

func (w *webClient) do(method, path string, body any, hdr map[string]string) (int, map[string]any) {
	w.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, w.base+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := w.c.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func openRig(t *testing.T) *rig {
	r := newRig(t, "ahmet")
	// newRig builds a closed server; open it for these tests.
	r.srvAccounts().open = true
	return r
}

// srvAccounts reaches the server behind the rig's handler.
func (r *rig) srvAccounts() *accountBook { return r.server.accounts }

func TestRegisterLoginAndAgentToken(t *testing.T) {
	r := openRig(t)
	w := newWebClient(t, r)
	code, out := w.do("POST", "/api/register", map[string]string{"nick": "Lilt", "password": "long enough pw"}, nil)
	if code != http.StatusCreated || out["nick"] != "lilt" {
		t.Fatalf("register = %d %v", code, out)
	}
	// Signed in by the cookie: make an agent token.
	code, out = w.do("POST", "/api/tokens", map[string]string{"label": "laptop"}, nil)
	token, _ := out["token"].(string)
	if code != http.StatusCreated || !strings.HasPrefix(token, "airc_") {
		t.Fatalf("token = %d %v", code, out)
	}
	// The agent speaks as lilt with that token.
	r.tokens["lilt"] = token
	if text, isErr := r.call("lilt", "say", map[string]any{"text": "hello from an account"}); isErr || !strings.Contains(text, "<lilt>") {
		t.Fatalf("say with account token: %q %v", text, isErr)
	}
	// Listing shows the label, never the token.
	code, out = w.do("GET", "/api/tokens", nil, nil)
	raw, _ := json.Marshal(out)
	if code != 200 || !strings.Contains(string(raw), "laptop") || strings.Contains(string(raw), token) {
		t.Fatalf("list = %d %s", code, raw)
	}
	id := out["tokens"].([]any)[0].(map[string]any)["id"].(string)
	// Revoked: the token stops working at once.
	if code, _ = w.do("DELETE", "/api/tokens", map[string]string{"id": id}, nil); code != 200 {
		t.Fatalf("revoke = %d", code)
	}
	if c, _ := r.rpc("lilt", "tools/list", nil); c != http.StatusUnauthorized {
		t.Fatalf("revoked token still works: %d", c)
	}
	// Nothing secret is in the file.
	file, _ := os.ReadFile(filepath.Join(r.dir, "data", "accounts.json"))
	if strings.Contains(string(file), "long enough pw") || strings.Contains(string(file), token) {
		t.Fatal("accounts.json holds a password or token in clear")
	}
	// Log out, then the cookie no longer works.
	w.do("POST", "/api/logout", map[string]string{}, nil)
	if code, _ = w.do("GET", "/api/tokens", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("after logout = %d", code)
	}
	// Log in again with the password.
	if code, _ = w.do("POST", "/api/login", map[string]string{"nick": "lilt", "password": "long enough pw"}, nil); code != 200 {
		t.Fatalf("login = %d", code)
	}
	if code, _ = w.do("POST", "/api/login", map[string]string{"nick": "lilt", "password": "wrong password"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", code)
	}
}

func TestRegisterRefusals(t *testing.T) {
	r := openRig(t)
	w := newWebClient(t, r)
	cases := []struct {
		nick, pw string
		code     int
	}{
		{"ahmet", "long enough pw", http.StatusConflict},   // in the tokens file
		{"admin", "long enough pw", http.StatusBadRequest}, // reserved
		{"kenwea", "long enough pw", http.StatusBadRequest},
		{"1abc", "long enough pw", http.StatusBadRequest}, // bad nick
		{"bob", "short", http.StatusBadRequest},
	}
	for _, c := range cases {
		if code, out := w.do("POST", "/api/register", map[string]string{"nick": c.nick, "password": c.pw}, nil); code != c.code {
			t.Errorf("register %q = %d %v, want %d", c.nick, code, out, c.code)
		}
	}
	if code, _ := w.do("POST", "/api/register", map[string]string{"nick": "bob", "password": "long enough pw"}, nil); code != 201 {
		t.Fatalf("bob = %d", code)
	}
	other := newWebClient(t, r)
	if code, _ := other.do("POST", "/api/register", map[string]string{"nick": "bob", "password": "another long pw"}, nil); code != http.StatusConflict {
		t.Fatalf("taken nick = %d", code)
	}
	// A cross-site post is refused.
	if code, _ := other.do("POST", "/api/login", map[string]string{"nick": "bob", "password": "long enough pw"},
		map[string]string{"Origin": "https://evil.example"}); code != http.StatusBadRequest {
		t.Fatalf("cross-site login = %d", code)
	}
}

func TestRegisterClosedServerAndRateLimit(t *testing.T) {
	r := newRig(t, "ahmet")
	w := newWebClient(t, r)
	if code, _ := w.do("POST", "/api/register", map[string]string{"nick": "bob", "password": "long enough pw"}, nil); code != http.StatusForbidden {
		t.Fatalf("closed server registered: %d", code)
	}
	r.srvAccounts().open = true
	got429 := false
	for i := 0; i < 8; i++ {
		code, _ := w.do("POST", "/api/register", map[string]string{"nick": "n" + string(rune('a'+i)), "password": "long enough pw"}, nil)
		if code == http.StatusTooManyRequests {
			got429 = true
		}
	}
	if !got429 {
		t.Fatal("eight registrations from one address were all allowed")
	}
}

func TestAccountsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	tok := EmptyTokens()
	b := openAccounts(dir, tok)
	b.open = true
	if err := b.Register("zed", "long enough pw", "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Login("zed", "long enough pw", "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	token, err := b.NewAgentToken("zed", "ci")
	if err != nil {
		t.Fatal(err)
	}
	tok2 := EmptyTokens()
	again := openAccounts(dir, tok2)
	if nick, ok := tok2.Nick(token); !ok || nick != "zed" {
		t.Fatalf("token lost on restart: %q %v", nick, ok)
	}
	if _, err := again.Login("zed", "long enough pw", "1.2.3.4"); err != nil {
		t.Fatalf("login after restart: %v", err)
	}
}
