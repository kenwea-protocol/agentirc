package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// People on the page (2026-11-13): private messages, a profile that shows
// only what its owner chose to show, and ignore. The operator asked for the
// mIRC way: pick a nick in the list, right-click, see their info, open a
// private chat, or ignore them.
//
// A private conversation is stored like a channel under the key "=a=b"
// (the two nicks sorted), which NormalizeChannel never produces, so it can
// never be read as a channel and never shows in a channel list. Only the
// two nicks can read it.

const (
	maxProfileField = 200
	maxIgnores      = 200
)

// Profile fields. Each is shown to others only when its owner ticks it.
var profileFields = []string{"name", "about", "location", "website", "pronouns"}

type profile struct {
	Fields map[string]string `json:"fields,omitempty"`
	Public map[string]bool   `json:"public,omitempty"`
	Ignore []string          `json:"ignore,omitempty"`
	// DMsFrom: "all" (default) or "nobody". Staff can always write.
	DMsFrom string `json:"dmsFrom,omitempty"`
}

type socialBook struct {
	mu     sync.Mutex
	file   string
	byNick map[string]*profile
}

func openSocial(dir string) *socialBook {
	b := &socialBook{file: filepath.Join(dir, "profiles.json"), byNick: map[string]*profile{}}
	if raw, err := os.ReadFile(b.file); err == nil {
		_ = json.Unmarshal(raw, &b.byNick)
	}
	return b
}

func (b *socialBook) saveLocked() error {
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

func (b *socialBook) getLocked(nick string) *profile {
	p, ok := b.byNick[nick]
	if !ok {
		p = &profile{Fields: map[string]string{}, Public: map[string]bool{}}
		b.byNick[nick] = p
	}
	if p.Fields == nil {
		p.Fields = map[string]string{}
	}
	if p.Public == nil {
		p.Public = map[string]bool{}
	}
	return p
}

// Ignores reports whether nick ignores other.
func (b *socialBook) Ignores(nick, other string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.byNick[nick]
	return ok && slices.Contains(p.Ignore, other)
}

func (b *socialBook) IgnoreList(nick string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p, ok := b.byNick[nick]; ok {
		return append([]string(nil), p.Ignore...)
	}
	return nil
}

func (b *socialBook) SetIgnore(nick, other string, on bool) error {
	if nick == other {
		return fmt.Errorf("%w: you cannot ignore yourself", ErrBadInput)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.getLocked(nick)
	if on {
		if !slices.Contains(p.Ignore, other) {
			if len(p.Ignore) >= maxIgnores {
				return fmt.Errorf("%w: at most %d ignored nicks", ErrBadInput, maxIgnores)
			}
			p.Ignore = append(p.Ignore, other)
		}
	} else {
		p.Ignore = slices.DeleteFunc(p.Ignore, func(n string) bool { return n == other })
	}
	return b.saveLocked()
}

// PublicProfile returns the fields nick chose to show.
func (b *socialBook) PublicProfile(nick string) map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]string{}
	if p, ok := b.byNick[nick]; ok {
		for _, f := range profileFields {
			if v := p.Fields[f]; v != "" && p.Public[f] {
				out[f] = v
			}
		}
	}
	return out
}

func (b *socialBook) OwnProfile(nick string) profile {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.getLocked(nick)
	out := profile{Fields: map[string]string{}, Public: map[string]bool{}, DMsFrom: p.DMsFrom}
	for _, f := range profileFields {
		out.Fields[f] = p.Fields[f]
		out.Public[f] = p.Public[f]
	}
	if out.DMsFrom == "" {
		out.DMsFrom = "all"
	}
	return out
}

func (b *socialBook) SaveProfile(nick string, fields map[string]string, public map[string]bool, dmsFrom string) error {
	if dmsFrom != "" && dmsFrom != "all" && dmsFrom != "nobody" {
		return fmt.Errorf("%w: dmsFrom is all or nobody", ErrBadInput)
	}
	clean := map[string]string{}
	for _, f := range profileFields {
		v := strings.TrimSpace(strings.ReplaceAll(fields[f], "\n", " "))
		if utf8.RuneCountInString(v) > maxProfileField {
			return fmt.Errorf("%w: %s is longer than %d characters", ErrBadInput, f, maxProfileField)
		}
		if f == "website" && v != "" && !strings.HasPrefix(v, "https://") && !strings.HasPrefix(v, "http://") {
			v = "https://" + v
		}
		clean[f] = v
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.getLocked(nick)
	p.Fields = clean
	p.Public = map[string]bool{}
	for _, f := range profileFields {
		p.Public[f] = public[f]
	}
	p.DMsFrom = dmsFrom
	return b.saveLocked()
}

func (b *socialBook) acceptsDMs(nick string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.byNick[nick]
	return !ok || p.DMsFrom != "nobody"
}

// dmKey is the store key of the conversation between a and b.
func dmKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return "=" + a + "=" + b
}

// dmPeer returns the other nick of a conversation key nick is part of.
func dmPeer(key, nick string) (string, bool) {
	parts := strings.Split(strings.TrimPrefix(key, "="), "=")
	if !strings.HasPrefix(key, "=") || len(parts) != 2 {
		return "", false
	}
	switch nick {
	case parts[0]:
		return parts[1], true
	case parts[1]:
		return parts[0], true
	}
	return "", false
}

// knownNick: someone who can receive a message here.
func (s *Server) knownNick(nick string) bool {
	return s.accounts.Exists(nick) || s.tokens.has(nick)
}

// sendDM stores a private line from nick to peer.
func (s *Server) sendDM(ctx context.Context, nick, peer, text string, action bool) (Line, error) {
	peer = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(peer, "@")))
	if !nickName.MatchString(peer) || !s.knownNick(peer) {
		return Line{}, fmt.Errorf("%w: no nick %q on this server", ErrBadInput, peer)
	}
	if peer == nick {
		return Line{}, fmt.Errorf("%w: that is you", ErrBadInput)
	}
	if s.tokens.Role(nick) == RoleGuest {
		return Line{}, errors.New("read-only guest: this nick can listen but not write")
	}
	staff := s.staff.rank(nick) > 0
	if !staff && (s.social.Ignores(peer, nick) || !s.social.acceptsDMs(peer)) {
		// Same answer either way, so an ignored nick cannot tell.
		return Line{}, fmt.Errorf("%s does not take private messages from you", peer)
	}
	if !s.limit.allow(nick) {
		return Line{}, errors.New("slow down: too many lines; wait a few seconds")
	}
	key := dmKey(nick, peer)
	if fromPage(ctx) || s.tokens.Role(nick) == RoleOwner {
		s.cost.humanLine(key)
	} else if err := s.cost.allowAgentLine(nick, key); err != nil {
		return Line{}, err
	}
	kind := "msg"
	if action {
		kind = "action"
	}
	return s.store.PostKey(key, nick, kind, text)
}

// dmList is the nick's conversations, newest first, with unread counts.
func (s *Server) dmList(nick string) []map[string]any {
	type conv struct {
		peer   string
		last   Line
		unread int
	}
	var convs []conv
	for _, key := range s.store.Keys() {
		peer, ok := dmPeer(key, nick)
		if !ok {
			continue
		}
		lines := s.store.Last(key, 1)
		if len(lines) == 0 {
			continue
		}
		unread := 0
		for _, l := range s.store.Since(key, s.readUpTo(nick, key), KeepInRAM) {
			if l.Nick != nick {
				unread++
			}
		}
		convs = append(convs, conv{peer, lines[0], unread})
	}
	sort.Slice(convs, func(i, j int) bool { return convs[i].last.ID > convs[j].last.ID })
	out := make([]map[string]any, 0, len(convs))
	for _, c := range convs {
		out = append(out, map[string]any{"nick": c.peer, "unread": c.unread, "ignored": s.social.Ignores(nick, c.peer)})
	}
	return out
}

// dropIgnored removes lines from nicks that nick ignores.
func (s *Server) dropIgnored(nick string, lines []Line) []Line {
	ign := s.social.IgnoreList(nick)
	if len(ign) == 0 {
		return lines
	}
	out := lines[:0:0]
	for _, l := range lines {
		if !slices.Contains(ign, l.Nick) {
			out = append(out, l)
		}
	}
	return out
}

func (s *Server) profileText(asker, nick string) string {
	nick = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(nick, "@")))
	text := s.whois(asker, nick)
	fields := s.social.PublicProfile(nick)
	for _, f := range profileFields {
		if v, ok := fields[f]; ok {
			text += "\n  " + f + ": " + v
		}
	}
	if len(fields) == 0 && s.knownNick(nick) {
		text += "\n  (no public profile)"
	}
	if s.social.Ignores(asker, nick) {
		text += "\n  You ignore " + nick + "."
	}
	return text
}

var socialTools = []map[string]any{
	{"name": "msg", "description": "Send a private message to a nick. Only the two of you can read the conversation.",
		"inputSchema": schema(map[string]any{"nick": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"}}, "nick", "text")},
	{"name": "dm_history", "description": "Read your private conversation with a nick (default the last 30 lines).",
		"inputSchema": schema(map[string]any{"nick": map[string]any{"type": "string"}, "lines": map[string]any{"type": "integer", "minimum": 1, "maximum": 200}}, "nick")},
	{"name": "dms", "description": "List your private conversations, newest first, with unread counts.",
		"inputSchema": schema(map[string]any{})},
	{"name": "profile", "description": "A nick's roles and the profile fields they chose to make public.",
		"inputSchema": schema(map[string]any{"nick": map[string]any{"type": "string"}}, "nick")},
	{"name": "ignore", "description": "Ignore a nick: their lines are hidden from you and they cannot send you private messages. on=false stops ignoring.",
		"inputSchema": schema(map[string]any{"nick": map[string]any{"type": "string"}, "on": map[string]any{"type": "boolean"}}, "nick")},
}

func init() { tools = append(tools, socialTools...) }

func (s *Server) callSocialTool(ctx context.Context, nick, name string, params json.RawMessage) (string, bool, error) {
	switch name {
	case "msg", "dm_history", "dms", "profile", "ignore":
	default:
		return "", false, nil
	}
	var p struct {
		Arguments struct {
			Nick, Text string
			Lines      int
			On         *bool
			Action     bool
		} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", true, errors.New("bad arguments")
	}
	a := p.Arguments
	s.store.Touch(nick)
	peer := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(a.Nick, "@")))
	switch name {
	case "msg":
		l, err := s.sendDM(ctx, nick, peer, a.Text, a.Action)
		if err != nil {
			return "", true, err
		}
		return "(to " + peer + ") " + l.String(), true, nil
	case "dm_history":
		if !nickName.MatchString(peer) {
			return "", true, fmt.Errorf("%w: name a nick", ErrBadInput)
		}
		n := a.Lines
		if n <= 0 {
			n = 30
		}
		key := dmKey(nick, peer)
		lines := s.dropIgnored(nick, s.store.Last(key, min(n, 200)))
		if len(lines) == 0 {
			return "(no private messages with " + peer + ")", true, nil
		}
		s.setCursor(nick, key, lines[len(lines)-1].ID)
		return render(lines), true, nil
	case "dms":
		list := s.dmList(nick)
		if len(list) == 0 {
			return "No private conversations.", true, nil
		}
		var b strings.Builder
		b.WriteString("Private conversations:")
		for _, c := range list {
			fmt.Fprintf(&b, "\n  %s  %d unread", c["nick"], c["unread"])
		}
		return b.String(), true, nil
	case "profile":
		return s.profileText(nick, peer), true, nil
	case "ignore":
		if !nickName.MatchString(peer) {
			return "", true, fmt.Errorf("%w: name a nick", ErrBadInput)
		}
		on := a.On == nil || *a.On
		if err := s.social.SetIgnore(nick, peer, on); err != nil {
			return "", true, err
		}
		if on {
			return "You ignore " + peer + " now: their lines are hidden from you and they cannot message you. /unignore " + peer + " undoes it.", true, nil
		}
		return "You no longer ignore " + peer + ".", true, nil
	}
	return "", true, nil
}

// handleDMs: GET /api/dms lists conversations; GET /api/profile?nick= the
// signed-in nick's own profile (no nick) or another's public one; POST
// /api/profile saves your own.
func (s *Server) handleDMs(w http.ResponseWriter, r *http.Request) {
	nick, ok := s.authBrowser(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"me": nick, "dms": s.dmList(nick), "ignore": s.social.IgnoreList(nick)})
}

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	nick, ok := s.authBrowser(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		if other := r.URL.Query().Get("nick"); other != "" {
			writeJSON(w, http.StatusOK, map[string]any{"text": s.profileText(nick, other)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"nick": nick, "profile": s.social.OwnProfile(nick), "fields": profileFields})
	case http.MethodPost:
		if !sameOrigin(r) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON from this site only"})
			return
		}
		var in struct {
			Fields  map[string]string `json:"fields"`
			Public  map[string]bool   `json:"public"`
			DMsFrom string            `json:"dmsFrom"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in) != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad JSON"})
			return
		}
		if err := s.social.SaveProfile(nick, in.Fields, in.Public, in.DMsFrom); err != nil {
			writeJSON(w, errorStatus(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET or POST"})
	}
}

// dmLines serves a private conversation to the page in the shape of
// /api/lines. The caller is always one of the two nicks: the key is built
// from the signed-in nick.
func (s *Server) dmLines(w http.ResponseWriter, r *http.Request, nick, peer string) {
	peer = strings.ToLower(strings.TrimSpace(peer))
	if !nickName.MatchString(peer) {
		http.Error(w, "bad nick", http.StatusBadRequest)
		return
	}
	key := dmKey(nick, peer)
	after := int64(0)
	fmt.Sscan(r.URL.Query().Get("after"), &after)
	var lines []Line
	if after == 0 {
		lines = s.store.Last(key, 300)
	} else if r.URL.Query().Get("wait") == "1" {
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		lines = s.store.Wait(ctx, key, after, 300)
		cancel()
	} else {
		lines = s.store.Since(key, after, 300)
	}
	if len(lines) > 0 {
		s.setCursor(nick, key, lines[len(lines)-1].ID)
	}
	lines = s.dropIgnored(nick, lines)
	s.store.Touch(nick)
	out := linesJSON{Me: nick, Channel: "@" + peer, Lines: []lineJSON{}}
	out.Nicks = []nickJSON{{Nick: nick}, {Nick: peer}}
	for _, l := range lines {
		out.Lines = append(out.Lines, lineJSON{ID: l.ID, Time: l.At.Local().Format("15:04"), Nick: l.Nick, Kind: l.Kind, Text: l.Text})
	}
	if len(lines) == 0 && after > 0 {
		// Keep the page's cursor where it was.
		out.Lines = []lineJSON{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// readUpTo is the last line nick has read under key, 0 when never: unlike
// cursor, a conversation never opened counts as wholly unread.
func (s *Server) readUpTo(nick, key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursors[nick+key]
}
