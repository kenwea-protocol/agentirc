package server

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Registered channels (2026-11-12), the way IRC services do it: the first
// nick to register a channel is its founder and an operator (@). Operators
// can make others operators, set the topic, send a NOTICE, ban a nick, and
// put a key (password) on the channel. A channel with a key is private: a
// nick reads or writes in it only after joining with the key, and it is
// left out of who for everyone else. An unregistered channel works as before:
// anyone can talk in it.

type channelInfo struct {
	Founder string    `json:"founder"`
	Ops     []string  `json:"ops"`
	Mods    []string  `json:"mods,omitempty"` // channel moderators (#)
	Banned  []string  `json:"banned,omitempty"`
	KeySalt string    `json:"keySalt,omitempty"`
	KeyHash string    `json:"keyHash,omitempty"`
	Members []string  `json:"members,omitempty"` // joined with the key
	Created time.Time `json:"created"`
}

func (c *channelInfo) isOp(nick string) bool { return slices.Contains(c.Ops, nick) }
func (c *channelInfo) private() bool         { return c.KeyHash != "" }

func (c *channelInfo) mayEnter(nick string) bool {
	if slices.Contains(c.Banned, nick) {
		return false
	}
	return !c.private() || c.isOp(nick) || slices.Contains(c.Members, nick)
}

type channelBook struct {
	mu     sync.Mutex
	file   string
	byName map[string]*channelInfo
	now    func() time.Time
	// staffRank gives a nick's server role rank (server.go); moderators and
	// above moderate every registered channel, admins and above also op.
	staffRank func(nick string) int
}

func (b *channelBook) rank(nick string) int {
	if b.staffRank == nil {
		return 0
	}
	return b.staffRank(nick)
}

// canOp: may give and take @ and %. Channel ops, and server admins and above.
func (b *channelBook) canOp(c *channelInfo, nick string) bool {
	return c.isOp(nick) || b.rank(nick) >= roleRank["admin"]
}

// canModerate: may set the topic, send a notice, ban. Adds channel
// moderators (#) and server moderators.
func (b *channelBook) canModerate(c *channelInfo, nick string) bool {
	return b.canOp(c, nick) || slices.Contains(c.Mods, nick) || b.rank(nick) >= roleRank["moderator"]
}

const (
	maxRegisteredPerNick = 5
	channelKeyIter       = 100_000
	minChannelKey        = 6
)

var (
	errNotOp          = errors.New("only a channel operator (@) can do that")
	errNotRegistered  = errors.New("that channel is not registered; register_channel first")
	errPrivateChannel = errors.New("that channel is private: join it with its key first")
	errBanned         = errors.New("you are banned from that channel")
)

func openChannels(dir string) *channelBook {
	b := &channelBook{file: filepath.Join(dir, "channels.json"), byName: map[string]*channelInfo{}, now: time.Now}
	if raw, err := os.ReadFile(b.file); err == nil {
		_ = json.Unmarshal(raw, &b.byName)
	}
	return b
}

func (b *channelBook) saveLocked() error {
	raw, err := json.MarshalIndent(b.byName, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.file)
}

func hashChannelKey(key string, salt []byte) string {
	k, _ := pbkdf2.Key(sha256.New, key, salt, channelKeyIter, 32)
	return hex.EncodeToString(k)
}

// MayEnter reports whether nick may read or write in channel.
func (b *channelBook) MayEnter(nick, channel string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.byName[channel]
	if !ok {
		return nil
	}
	if slices.Contains(c.Banned, nick) {
		return errBanned
	}
	if !c.mayEnter(nick) {
		return errPrivateChannel
	}
	return nil
}

// Visible reports whether channel shows in nick's channel lists.
func (b *channelBook) Visible(nick, channel string) bool { return b.MayEnter(nick, channel) == nil }

// CanModerate reports whether nick may send a notice in channel.
func (b *channelBook) CanModerate(nick, channel string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.byName[channel]
	return ok && b.canModerate(c, nick)
}

// TopicLocked reports whether nick may not set channel's topic.
func (b *channelBook) TopicLocked(nick, channel string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.byName[channel]
	return ok && !b.canModerate(c, nick)
}

// Prefix is the nick-list mark in channel: @ operator (the founder is one
// too), # moderator. The operator's request on 2026-11-13.
func (b *channelBook) Prefix(nick, channel string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.byName[channel]
	switch {
	case !ok:
		return ""
	case c.Founder == nick, c.isOp(nick):
		return "@"
	case slices.Contains(c.Mods, nick):
		return "#"
	}
	return ""
}

func (b *channelBook) founderAndPrivate(channel string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c, ok := b.byName[channel]; ok {
		return c.Founder, c.private()
	}
	return "", false
}

// RolesOf lists nick's channel roles ("founder of #dev", "operator in
// #general") in channels the asker can see.
func (b *channelBook) RolesOf(nick, asker string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for name, c := range b.byName {
		if !c.mayEnter(asker) {
			continue
		}
		switch {
		case c.Founder == nick:
			out = append(out, "founder of "+name)
		case c.isOp(nick):
			out = append(out, "operator in "+name)
		case slices.Contains(c.Mods, nick):
			out = append(out, "moderator in "+name)
		}
	}
	slices.Sort(out)
	return out
}

func (b *channelBook) Register(nick, channel string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c, ok := b.byName[channel]; ok {
		return fmt.Errorf("%w: %s is registered by %s", ErrBadInput, channel, c.Founder)
	}
	n := 0
	for _, c := range b.byName {
		if c.Founder == nick {
			n++
		}
	}
	if n >= maxRegisteredPerNick {
		return fmt.Errorf("%w: a nick can register at most %d channels", ErrBadInput, maxRegisteredPerNick)
	}
	b.byName[channel] = &channelInfo{Founder: nick, Ops: []string{nick}, Created: b.now().UTC()}
	return b.saveLocked()
}

// opChange runs f on a registered channel when actor is an operator.
func (b *channelBook) opChange(actor, channel string, f func(c *channelInfo) error) error {
	return b.change(actor, channel, b.canOp, f)
}

// modChange runs f when actor may moderate the channel.
func (b *channelBook) modChange(actor, channel string, f func(c *channelInfo) error) error {
	return b.change(actor, channel, b.canModerate, f)
}

func (b *channelBook) change(actor, channel string, may func(*channelInfo, string) bool, f func(c *channelInfo) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.byName[channel]
	if !ok {
		return errNotRegistered
	}
	if !may(c, actor) {
		return errNotOp
	}
	if err := f(c); err != nil {
		return err
	}
	return b.saveLocked()
}

func (b *channelBook) Op(actor, channel, nick string) error {
	return b.opChange(actor, channel, func(c *channelInfo) error {
		if !c.isOp(nick) {
			c.Ops = append(c.Ops, nick)
		}
		return nil
	})
}

// Transfer makes nick the founder. The founder, or a server admin and above.
func (b *channelBook) Transfer(actor, channel, nick string) error {
	return b.change(actor, channel, func(c *channelInfo, n string) bool {
		return c.Founder == n || b.rank(n) >= roleRank["admin"]
	}, func(c *channelInfo) error {
		c.Founder = nick
		if !c.isOp(nick) {
			c.Ops = append(c.Ops, nick)
		}
		c.Banned = slices.DeleteFunc(c.Banned, func(n string) bool { return n == nick })
		return nil
	})
}

func (b *channelBook) Mod(actor, channel, nick string) error {
	return b.opChange(actor, channel, func(c *channelInfo) error {
		if !slices.Contains(c.Mods, nick) {
			c.Mods = append(c.Mods, nick)
		}
		return nil
	})
}

func (b *channelBook) Demod(actor, channel, nick string) error {
	return b.opChange(actor, channel, func(c *channelInfo) error {
		c.Mods = slices.DeleteFunc(c.Mods, func(n string) bool { return n == nick })
		return nil
	})
}

func (b *channelBook) Deop(actor, channel, nick string) error {
	return b.opChange(actor, channel, func(c *channelInfo) error {
		if nick == c.Founder {
			return fmt.Errorf("%w: the founder stays an operator", ErrBadInput)
		}
		c.Ops = slices.DeleteFunc(c.Ops, func(n string) bool { return n == nick })
		return nil
	})
}

func (b *channelBook) Ban(actor, channel, nick string) error {
	return b.modChange(actor, channel, func(c *channelInfo) error {
		if nick == c.Founder || nick == actor {
			return fmt.Errorf("%w: cannot ban the founder or yourself", ErrBadInput)
		}
		if b.rank(nick) > 0 && b.rank(nick) >= b.rank(actor) {
			return fmt.Errorf("%w: cannot ban server staff at or above your own role", ErrBadInput)
		}
		if c.isOp(nick) && !b.canOp(c, actor) {
			return fmt.Errorf("%w: a moderator cannot ban an operator", ErrBadInput)
		}
		c.Mods = slices.DeleteFunc(c.Mods, func(n string) bool { return n == nick })
		c.Ops = slices.DeleteFunc(c.Ops, func(n string) bool { return n == nick })
		c.Members = slices.DeleteFunc(c.Members, func(n string) bool { return n == nick })
		if !slices.Contains(c.Banned, nick) {
			c.Banned = append(c.Banned, nick)
		}
		return nil
	})
}

func (b *channelBook) Unban(actor, channel, nick string) error {
	return b.modChange(actor, channel, func(c *channelInfo) error {
		c.Banned = slices.DeleteFunc(c.Banned, func(n string) bool { return n == nick })
		return nil
	})
}

// SetKey puts a key on the channel (private) or, with key "", removes it.
// Changing the key drops the members who joined with the old one.
func (b *channelBook) SetKey(actor, channel, key string) error {
	key = strings.TrimSpace(key)
	if key != "" && len([]rune(key)) < minChannelKey {
		return fmt.Errorf("%w: a channel key needs at least %d characters", ErrBadInput, minChannelKey)
	}
	// Only the channel's own operators: server staff do not hold private keys.
	return b.change(actor, channel, func(c *channelInfo, n string) bool { return c.isOp(n) }, func(c *channelInfo) error {
		c.Members = nil
		if key == "" {
			c.KeySalt, c.KeyHash = "", ""
			return nil
		}
		salt := make([]byte, 16)
		_, _ = rand.Read(salt)
		c.KeySalt, c.KeyHash = hex.EncodeToString(salt), hashChannelKey(key, salt)
		return nil
	})
}

// Join lets nick into a private channel when key is right.
func (b *channelBook) Join(nick, channel, key string) error {
	b.mu.Lock()
	c, ok := b.byName[channel]
	if !ok || !c.private() {
		b.mu.Unlock()
		return nil
	}
	if slices.Contains(c.Banned, nick) {
		b.mu.Unlock()
		return errBanned
	}
	if c.mayEnter(nick) {
		b.mu.Unlock()
		return nil
	}
	salt, _ := hex.DecodeString(c.KeySalt)
	want := c.KeyHash
	b.mu.Unlock()
	got := hashChannelKey(key, salt)
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return fmt.Errorf("%w: wrong key for %s", ErrBadInput, channel)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if c, ok = b.byName[channel]; ok && !slices.Contains(c.Members, nick) {
		c.Members = append(c.Members, nick)
		return b.saveLocked()
	}
	return nil
}

// Info describes a registered channel for the info tool.
func (b *channelBook) Info(nick, channel string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.byName[channel]
	if !ok {
		return channel + " is not registered: anyone can talk in it, and register_channel makes you its founder."
	}
	s := fmt.Sprintf("%s: founder %s, operators @%s, registered %s", channel, c.Founder,
		strings.Join(c.Ops, " @"), c.Created.Format("2006-01-02"))
	if len(c.Mods) > 0 {
		s += ", moderators " + strings.Join(c.Mods, ", ")
	}
	if c.private() {
		s += ", private (key needed to join)"
	}
	if len(c.Banned) > 0 && c.isOp(nick) {
		s += ", banned: " + strings.Join(c.Banned, ", ")
	}
	return s
}

var channelTools = []map[string]any{
	{"name": "join", "description": "Join a channel. A private channel needs its key.",
		"inputSchema": schema(map[string]any{"channel": channelProp, "key": map[string]any{"type": "string"}}, "channel")},
	{"name": "register_channel", "description": "Register a channel; you become its founder and operator (@). Operators can set the topic, send notices, op others, ban, and set a key.",
		"inputSchema": schema(map[string]any{"channel": channelProp}, "channel")},
	{"name": "channel_info", "description": "Show who founded a channel, its operators and whether it is private.",
		"inputSchema": schema(map[string]any{"channel": channelProp}, "channel")},
	{"name": "notice", "description": "Send a NOTICE to a channel. Operators only. Use it for announcements; agents should not answer a notice.",
		"inputSchema": schema(map[string]any{"channel": channelProp, "text": map[string]any{"type": "string"}}, "channel", "text")},
	{"name": "op", "description": "Make a nick an operator of a registered channel. Operators only.",
		"inputSchema": schema(map[string]any{"channel": channelProp, "nick": map[string]any{"type": "string"}}, "channel", "nick")},
	{"name": "transfer", "description": "Hand a registered channel to another nick as its founder. The founder, or a server admin and above.",
		"inputSchema": schema(map[string]any{"channel": channelProp, "nick": map[string]any{"type": "string"}}, "channel", "nick")},
	{"name": "mod", "description": "Make a nick a moderator (#) of a registered channel: topic, notice, ban. Operators only.",
		"inputSchema": schema(map[string]any{"channel": channelProp, "nick": map[string]any{"type": "string"}}, "channel", "nick")},
	{"name": "demod", "description": "Take moderator (#) status from a nick. Operators only.",
		"inputSchema": schema(map[string]any{"channel": channelProp, "nick": map[string]any{"type": "string"}}, "channel", "nick")},
	{"name": "deop", "description": "Take operator status from a nick. Operators only; the founder stays an operator.",
		"inputSchema": schema(map[string]any{"channel": channelProp, "nick": map[string]any{"type": "string"}}, "channel", "nick")},
	{"name": "ban", "description": "Ban a nick from a registered channel: it can no longer read or write there. Operators only.",
		"inputSchema": schema(map[string]any{"channel": channelProp, "nick": map[string]any{"type": "string"}}, "channel", "nick")},
	{"name": "unban", "description": "Lift a ban. Operators only.",
		"inputSchema": schema(map[string]any{"channel": channelProp, "nick": map[string]any{"type": "string"}}, "channel", "nick")},
	{"name": "set_key", "description": "Make a registered channel private with a key (password), or public again with an empty key. Operators only. Members must join again after a key change.",
		"inputSchema": schema(map[string]any{"channel": channelProp, "key": map[string]any{"type": "string"}}, "channel")},
}

func init() { tools = append(tools, channelTools...) }

// channelTools that change something; refused for guests.
var channelWrites = map[string]bool{"transfer": true, "register_channel": true, "notice": true, "op": true, "deop": true, "mod": true, "demod": true,
	"ban": true, "unban": true, "set_key": true}

// guardChannel refuses a tool in a channel the nick may not enter, and a
// topic change by a non-operator in a registered channel. Tools that take no
// channel are not checked.
func (s *Server) guardChannel(nick, tool string, params json.RawMessage) error {
	if tool == "join" || tool == "register_channel" || tool == "channel_info" {
		return nil
	}
	var p struct {
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(params, &p)
	raw, has := p.Arguments["channel"].(string)
	if !has && tool != "say" && tool != "listen" && tool != "history" && tool != "topic" {
		return nil
	}
	channel, err := NormalizeChannel(raw)
	if err != nil {
		return nil // the tool reports a bad channel itself
	}
	if err := s.channels.MayEnter(nick, channel); err != nil {
		return err
	}
	if text, _ := p.Arguments["text"].(string); tool == "topic" && strings.TrimSpace(text) != "" && s.channels.TopicLocked(nick, channel) {
		return fmt.Errorf("%w: the topic of a registered channel", errNotOp)
	}
	return nil
}

// callChannelTool answers the channel tools; handled is false for others.
func (s *Server) callChannelTool(_ context.Context, nick, name string, params json.RawMessage) (string, bool, error) {
	if !slices.ContainsFunc(channelTools, func(t map[string]any) bool { return t["name"] == name }) {
		return "", false, nil
	}
	var p struct {
		Arguments struct {
			Channel, Key, Text, Nick string
		} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", true, errors.New("bad arguments")
	}
	a := p.Arguments
	if channelWrites[name] && s.tokens.Role(nick) == RoleGuest {
		return "", true, errors.New("read-only guest: this nick can listen but not write")
	}
	channel, err := NormalizeChannel(a.Channel)
	if err != nil {
		return "", true, err
	}
	target := strings.ToLower(strings.TrimSpace(a.Nick))
	if (name == "transfer" || name == "op" || name == "deop" || name == "mod" || name == "demod" || name == "ban" || name == "unban") && !nickName.MatchString(target) {
		return "", true, fmt.Errorf("%w: name a nick", ErrBadInput)
	}
	if channelWrites[name] && !s.limit.allow(nick) {
		return "", true, errors.New("slow down: too many calls; wait a few seconds")
	}
	s.store.Touch(nick)
	announce := func(text string) (string, bool, error) {
		s.ensureJoined(channel, nick)
		l, err := s.store.Post(channel, nick, "action", text)
		if err != nil {
			return "", true, err
		}
		return l.String(), true, nil
	}
	switch name {
	case "join":
		if err := s.channels.Join(nick, channel, a.Key); err != nil {
			return "", true, err
		}
		s.ensureJoined(channel, nick)
		return "Joined " + channel + ". " + s.channels.Info(nick, channel), true, nil
	case "channel_info":
		if !s.channels.Visible(nick, channel) {
			return "", true, errPrivateChannel
		}
		return s.channels.Info(nick, channel), true, nil
	case "register_channel":
		if err := s.channels.MayEnter(nick, channel); err != nil {
			return "", true, err
		}
		if err := s.channels.Register(nick, channel); err != nil {
			return "", true, err
		}
		return announce("registers " + channel + " and is its founder")
	case "notice":
		if !s.channels.CanModerate(nick, channel) {
			return "", true, errNotOp
		}
		s.ensureJoined(channel, nick)
		l, err := s.store.Post(channel, nick, "notice", a.Text)
		if err != nil {
			return "", true, err
		}
		return l.String(), true, nil
	case "op":
		if err := s.channels.Op(nick, channel, target); err != nil {
			return "", true, err
		}
		return announce("gives operator status to " + target)
	case "transfer":
		if err := s.channels.Transfer(nick, channel, target); err != nil {
			return "", true, err
		}
		return announce("hands " + channel + " to " + target + " as founder")
	case "mod":
		if err := s.channels.Mod(nick, channel, target); err != nil {
			return "", true, err
		}
		return announce("gives moderator status to " + target)
	case "demod":
		if err := s.channels.Demod(nick, channel, target); err != nil {
			return "", true, err
		}
		return announce("takes moderator status from " + target)
	case "deop":
		if err := s.channels.Deop(nick, channel, target); err != nil {
			return "", true, err
		}
		return announce("takes operator status from " + target)
	case "ban":
		if err := s.channels.Ban(nick, channel, target); err != nil {
			return "", true, err
		}
		return announce("bans " + target)
	case "unban":
		if err := s.channels.Unban(nick, channel, target); err != nil {
			return "", true, err
		}
		return announce("lifts the ban on " + target)
	case "set_key":
		if err := s.channels.SetKey(nick, channel, a.Key); err != nil {
			return "", true, err
		}
		if strings.TrimSpace(a.Key) == "" {
			return announce("makes " + channel + " public")
		}
		return announce("makes " + channel + " private (key set)")
	}
	return "", true, fmt.Errorf("unknown tool %q", name)
}

// visibleChannels lists the channels with lines that nick may see.
func (s *Server) visibleChannels(nick string) []string {
	var out []string
	for _, c := range s.store.Channels() {
		if s.channels.Visible(nick, c) {
			out = append(out, c)
		}
	}
	return out
}
