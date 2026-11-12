package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed page.html
var pageHTML []byte

// The owner's window. The browser's own login prompt (HTTP Basic, any user
// name, the token as password) is the only sign-in; the page then talks to
// /api/* on the same origin, which the browser authenticates the same way.

func (s *Server) authBrowser(w http.ResponseWriter, r *http.Request) (string, bool) {
	// The sign-in page's cookie first (accounts.go), then a token sent by a
	// script. No Basic challenge any more: the browser's own login box asked
	// for a "username" nobody had.
	if nick, ok := s.accounts.FromCookie(r); ok {
		return nick, true
	}
	nick, ok := s.tokens.FromRequest(r)
	if !ok {
		http.Error(w, "unauthorized: sign in first", http.StatusUnauthorized)
	}
	return nick, ok
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "" {
		http.NotFound(w, r)
		return
	}
	if _, ok := s.accounts.FromCookie(r); !ok {
		if _, ok := s.tokens.FromRequest(r); !ok {
			servePage(w, signinHTML) // accounts.go
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'")
	_, _ = w.Write(pageHTML)
}

type lineJSON struct {
	ID   int64  `json:"id"`
	Time string `json:"time"`
	Nick string `json:"nick"`
	Kind string `json:"kind"`
	Text string `json:"text"`
	Ref  int64  `json:"ref,omitempty"`
}

// nickJSON is one entry of the nick list; idle means no activity for 15
// minutes, shown greyed rather than hidden, as an away user is in mIRC.
type nickJSON struct {
	Nick   string `json:"nick"`
	Idle   bool   `json:"idle"`
	Prefix string `json:"prefix,omitempty"` // ~ founder, @ op, % moderator of this channel
	Role   string `json:"role,omitempty"`   // server role (server.go)
}

type linesJSON struct {
	Me       string     `json:"me"`
	Channel  string     `json:"channel"`
	Topic    string     `json:"topic"`
	Channels []string   `json:"channels"`
	Nicks    []nickJSON `json:"nicks"`
	Lines    []lineJSON `json:"lines"`
}

// handleLines: GET ?channel=#x&after=ID[&wait=1] — new lines, long-polled.
func (s *Server) handleLines(w http.ResponseWriter, r *http.Request) {
	nick, ok := s.authBrowser(w, r)
	if !ok {
		return
	}
	channel, err := NormalizeChannel(r.URL.Query().Get("channel"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.channels.MayEnter(nick, channel); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	var lines []Line
	if after == 0 {
		lines = s.store.Last(channel, 300)
	} else if r.URL.Query().Get("wait") == "1" {
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		lines = s.store.Wait(ctx, channel, after, 300)
		cancel()
	} else {
		lines = s.store.Since(channel, after, 300)
	}
	s.store.Touch(nick)
	out := linesJSON{Me: nick, Channel: channel, Topic: s.store.Topic(channel),
		Channels: s.visibleChannels(nick), Lines: []lineJSON{}}
	sort.Strings(out.Channels)
	for n, at := range s.store.Seen() {
		out.Nicks = append(out.Nicks, nickJSON{Nick: n, Idle: time.Since(at) > 15*time.Minute,
			Prefix: s.channels.Prefix(n, channel), Role: s.staff.Role(n)})
	}
	rankOf := map[string]int{"~": 3, "@": 2, "%": 1}
	sort.Slice(out.Nicks, func(i, j int) bool {
		a, b := out.Nicks[i], out.Nicks[j]
		if rankOf[a.Prefix] != rankOf[b.Prefix] {
			return rankOf[a.Prefix] > rankOf[b.Prefix]
		}
		return a.Nick < b.Nick
	})
	for _, l := range lines {
		out.Lines = append(out.Lines, lineJSON{ID: l.ID, Time: l.At.Local().Format("15:04"),
			Nick: l.Nick, Kind: l.Kind, Text: l.Text, Ref: l.Ref})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleSay: POST {"channel","text"}. "/me x" is an action and "/topic x"
// sets the topic, as in mIRC. The custom header makes a cross-site form
// post impossible (it would need a CORS preflight this server never
// answers), so a page elsewhere cannot speak with the owner's credentials.
func (s *Server) handleSay(w http.ResponseWriter, r *http.Request) {
	nick, ok := s.authBrowser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost || r.Header.Get("X-AgentIRC") != "1" {
		http.Error(w, "POST with X-AgentIRC: 1", http.StatusForbidden)
		return
	}
	var in struct {
		Channel string `json:"channel"`
		Text    string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if s.tokens.Role(nick) == RoleGuest {
		http.Error(w, "read-only guest: this nick can listen but not write", http.StatusForbidden)
		return
	}
	if !s.limit.allow(nick) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
		return
	}
	// "/approve 12 note" and "/deny 12 note": the owner decides an ask.
	if decision, rest, ok := ownerCommand(in.Text); ok {
		if s.tokens.Role(nick) != RoleOwner {
			http.Error(w, "only the owner can answer an ask", http.StatusForbidden)
			return
		}
		idText, note, _ := strings.Cut(strings.TrimSpace(rest), " ")
		id, err := strconv.ParseInt(strings.TrimPrefix(idText, "#"), 10, 64)
		if err != nil {
			http.Error(w, "usage: /"+decision+" <ask id> [note]", http.StatusBadRequest)
			return
		}
		if _, err := s.decide(nick, id, decision, strings.TrimSpace(note)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	kind, text := "msg", in.Text
	if rest, ok := strings.CutPrefix(text, "/me "); ok {
		kind, text = "action", rest
	} else if rest, ok := strings.CutPrefix(text, "/topic "); ok {
		kind, text = "topic", rest
	}
	channel, err := NormalizeChannel(in.Channel)
	if err == nil {
		err = s.channels.MayEnter(nick, channel)
	}
	if err == nil && kind == "topic" && s.channels.TopicLocked(nick, channel) {
		err = errNotOp
	}
	if err == nil {
		s.ensureJoined(channel, nick)
		_, err = s.store.Post(channel, nick, kind, text)
		if err == nil {
			s.cost.humanLine(channel) // a person wrote on the page
		}
	}
	if errors.Is(err, errPrivateChannel) || errors.Is(err, errBanned) || errors.Is(err, errNotOp) {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if errors.Is(err, ErrBadInput) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ownerCommand recognises "/approve ..." and "/deny ...".
func ownerCommand(text string) (decision, rest string, ok bool) {
	for _, d := range []string{"approve", "deny"} {
		if r, found := strings.CutPrefix(text, "/"+d+" "); found {
			return d, r, true
		}
	}
	return "", "", false
}
