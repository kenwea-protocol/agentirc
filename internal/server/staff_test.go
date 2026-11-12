package server

import (
	"net/http"
	"strings"
	"testing"
)

func staffRig(t *testing.T) *rig {
	r := newRig(t, "ahmet", "kenwea", "win", "eve")
	if err := r.server.staff.Bootstrap(map[string]string{"ahmet": "founder", "kenwea": "superadmin", "win": "techadmin"}); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestServerInfoListsStaffAndVersion(t *testing.T) {
	r := staffRig(t)
	text, isErr := r.call("eve", "server_info", map[string]any{})
	for _, want := range []string{"Kenwea AgentIRC", ServerVersion, "founder", "ahmet", "superadmin", "kenwea", "/help"} {
		if isErr || !strings.Contains(text, want) {
			t.Fatalf("server_info lacks %q: %q", want, text)
		}
	}
	if text, _ = r.call("eve", "help", map[string]any{}); !strings.Contains(text, "/register") || !strings.Contains(text, "/mod nick") {
		t.Fatalf("help: %q", text)
	}
}

func TestRolesOnlyGoDownward(t *testing.T) {
	r := staffRig(t)
	if text, isErr := r.call("eve", "set_role", map[string]any{"nick": "win", "role": "moderator"}); !isErr {
		t.Fatalf("a user changed a role: %q", text)
	}
	if text, isErr := r.call("kenwea", "set_role", map[string]any{"nick": "ahmet", "role": "none"}); !isErr {
		t.Fatalf("superadmin removed the founder: %q", text)
	}
	if text, isErr := r.call("kenwea", "set_role", map[string]any{"nick": "eve", "role": "superadmin"}); !isErr {
		t.Fatalf("superadmin gave its own rank: %q", text)
	}
	if text, isErr := r.call("kenwea", "set_role", map[string]any{"nick": "eve", "role": "moderator"}); isErr {
		t.Fatalf("superadmin could not make a moderator: %q", text)
	}
	if text, _ := r.call("ahmet", "whois", map[string]any{"nick": "eve"}); !strings.Contains(text, "moderator") {
		t.Fatalf("whois: %q", text)
	}
}

func TestStaffModeratorModeratesRegisteredChannels(t *testing.T) {
	r := staffRig(t)
	r.call("win", "register_channel", map[string]any{"channel": "#dev"})
	if _, isErr := r.call("eve", "notice", map[string]any{"channel": "#dev", "text": "hi"}); !isErr {
		t.Fatal("a plain user sent a notice")
	}
	r.call("kenwea", "set_role", map[string]any{"nick": "eve", "role": "moderator"})
	if text, isErr := r.call("eve", "notice", map[string]any{"channel": "#dev", "text": "be kind"}); isErr {
		t.Fatalf("server moderator could not notice: %q", text)
	}
	// A server moderator is not a channel op: it cannot op others.
	if _, isErr := r.call("eve", "op", map[string]any{"channel": "#dev", "nick": "eve"}); !isErr {
		t.Fatal("server moderator gave itself op")
	}
	// Nor ban staff above it.
	if _, isErr := r.call("eve", "ban", map[string]any{"channel": "#dev", "nick": "kenwea"}); !isErr {
		t.Fatal("moderator banned a superadmin")
	}
}

func TestChannelModeratorPrefixAndPowers(t *testing.T) {
	r := staffRig(t)
	r.call("eve", "register_channel", map[string]any{"channel": "#fun"})
	r.call("eve", "op", map[string]any{"channel": "#fun", "nick": "win"})
	r.call("eve", "mod", map[string]any{"channel": "#fun", "nick": "kenwea"})
	b := r.server.channels
	if b.Prefix("eve", "#fun") != "@" || b.Prefix("win", "#fun") != "@" || b.Prefix("kenwea", "#fun") != "#" || b.Prefix("ahmet", "#fun") != "" {
		t.Fatal("prefixes wrong")
	}
	if text, _ := r.call("ahmet", "channel_info", map[string]any{"channel": "#fun"}); !strings.Contains(text, "moderators kenwea") {
		t.Fatalf("info: %q", text)
	}
}

func TestInviteReservesNickUntilClaimed(t *testing.T) {
	r := staffRig(t)
	r.server.accounts.open = true
	text, isErr := r.call("kenwea", "invite_staff", map[string]any{"nick": "hobosentinel", "role": "moderator"})
	if isErr || !strings.Contains(text, "claim_") {
		t.Fatalf("invite: %q", text)
	}
	code := text[strings.Index(text, "claim_"):]
	code = strings.Fields(code)[0]
	w := newWebClient(t, r)
	// Nobody else can register the reserved nick.
	if c, _ := w.do("POST", "/api/register", map[string]string{"nick": "hobosentinel", "password": "long enough pw"}, nil); c != http.StatusConflict {
		t.Fatalf("reserved nick registered: %d", c)
	}
	if c, _ := w.do("POST", "/api/claim", map[string]string{"nick": "hobosentinel", "code": "claim_wrong", "password": "long enough pw"}, nil); c != http.StatusUnauthorized {
		t.Fatalf("wrong code = %d", c)
	}
	c, out := w.do("POST", "/api/claim", map[string]string{"nick": "hobosentinel", "code": code, "password": "long enough pw"}, nil)
	if c != http.StatusCreated || out["role"] != "moderator" {
		t.Fatalf("claim = %d %v", c, out)
	}
	if r.server.staff.Role("hobosentinel") != "moderator" {
		t.Fatal("role not given")
	}
	// The code works once.
	if c, _ := newWebClient(t, r).do("POST", "/api/claim", map[string]string{"nick": "hobosentinel", "code": code, "password": "other long pw"}, nil); c != http.StatusUnauthorized {
		t.Fatalf("code reused = %d", c)
	}
}

func TestPageCommandsRunTheTools(t *testing.T) {
	cases := map[string]string{
		"/help": "help", "/server": "server_info", "/list dev": "list_channels", "/whois ahmet": "whois",
		"/join #dev key1": "join", "/register": "register_channel", "/op win": "op", "/mod eve": "mod",
		"/ban eve": "ban", "/notice hello": "notice", "/key s3cret": "set_key", "/setrole eve helper": "set_role",
		"/invite bob helper": "invite_staff", "/info": "channel_info",
	}
	for in, want := range cases {
		tool, args, err := commandTool("#dev", in)
		if err != nil || tool != want {
			t.Errorf("%q -> %q %v %v, want %q", in, tool, args, err, want)
		}
	}
	if _, _, err := commandTool("#dev", "/frobnicate"); err == nil || !strings.Contains(err.Error(), "/help") {
		t.Errorf("unknown command: %v", err)
	}
	if _, _, err := commandTool("#dev", "/op"); err == nil {
		t.Error("/op without a nick accepted")
	}
}

func TestListChannelsFindsAndHidesPrivate(t *testing.T) {
	r := staffRig(t)
	r.call("win", "say", map[string]any{"channel": "#golang", "text": "hi"})
	r.call("win", "topic", map[string]any{"channel": "#golang", "text": "Go help and chatter"})
	r.call("win", "register_channel", map[string]any{"channel": "#secret"})
	r.call("win", "set_key", map[string]any{"channel": "#secret", "key": "letmein!"})
	text, _ := r.call("eve", "list_channels", map[string]any{"query": "go"})
	if !strings.Contains(text, "#golang") || !strings.Contains(text, "Go help") {
		t.Fatalf("list: %q", text)
	}
	if text, _ = r.call("eve", "list_channels", map[string]any{}); strings.Contains(text, "#secret") {
		t.Fatalf("private channel listed to outsider: %q", text)
	}
}
