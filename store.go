package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Line is one line in a channel, the way mIRC shows it: a time, a nick and
// what was said. Kind is "msg", "action" (/me), "topic" or "join".
type Line struct {
	ID      int64  `json:"id"`
	Channel string `json:"channel"`
	Nick    string `json:"nick"`
	Kind    string `json:"kind"`
	// Ref points at another line: an approve/deny line refers to its ask.
	Ref  int64     `json:"ref,omitempty"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// String renders a line mIRC-style: "[21:55] <win> hello".
func (l Line) String() string {
	ts := l.At.Local().Format("15:04")
	switch l.Kind {
	case "action":
		return fmt.Sprintf("[%s] * %s %s", ts, l.Nick, l.Text)
	case "topic":
		return fmt.Sprintf("[%s] *** %s changes topic to '%s'", ts, l.Nick, l.Text)
	case "notice":
		return fmt.Sprintf("[%s] -%s:%s- %s", ts, l.Nick, l.Channel, l.Text)
	case "join":
		return fmt.Sprintf("[%s] *** %s has joined %s", ts, l.Nick, l.Channel)
	case "ask":
		return fmt.Sprintf("[%s] ?? <%s> asks #%d: %s", ts, l.Nick, l.ID, l.Text)
	case "approve", "deny":
		verb := "approved"
		if l.Kind == "deny" {
			verb = "denied"
		}
		if l.Text == "" {
			return fmt.Sprintf("[%s] !! %s %s ask #%d", ts, l.Nick, verb, l.Ref)
		}
		return fmt.Sprintf("[%s] !! %s %s ask #%d: %s", ts, l.Nick, verb, l.Ref, l.Text)
	default:
		return fmt.Sprintf("[%s] <%s> %s", ts, l.Nick, l.Text)
	}
}

const (
	maxText     = 4000 // characters per line
	keepInRAM   = 5000 // lines per channel served from memory
	maxChannels = 20
)

var channelName = regexp.MustCompile(`^#[a-z0-9_-]{1,32}$`)

// Store keeps every channel in memory and appends each line to a JSONL
// file, so a restart loses nothing. One mutex: two to three speakers do not
// need more.
type Store struct {
	mu      sync.Mutex
	dir     string
	nextID  int64
	lines   map[string][]Line
	topics  map[string]string
	seen    map[string]time.Time // nick -> last activity
	changed chan struct{}        // closed and replaced on every new line
	file    *os.File
	now     func() time.Time
}

// OpenStore loads dir/log.jsonl (if any) and opens it for appending.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{
		dir:     dir,
		nextID:  1,
		lines:   map[string][]Line{},
		topics:  map[string]string{},
		seen:    map[string]time.Time{},
		changed: make(chan struct{}),
		now:     time.Now,
	}
	path := filepath.Join(dir, "log.jsonl")
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			var l Line
			if json.Unmarshal(sc.Bytes(), &l) == nil {
				s.remember(l)
			}
		}
		f.Close()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	s.file = f
	return s, nil
}

func (s *Store) remember(l Line) {
	ch := s.lines[l.Channel]
	ch = append(ch, l)
	if len(ch) > keepInRAM {
		ch = ch[len(ch)-keepInRAM:]
	}
	s.lines[l.Channel] = ch
	if l.Kind == "topic" {
		s.topics[l.Channel] = l.Text
	}
	if l.At.After(s.seen[l.Nick]) {
		s.seen[l.Nick] = l.At
	}
	if l.ID >= s.nextID {
		s.nextID = l.ID + 1
	}
}

// ErrBadInput is returned for an invalid channel, empty or oversized text.
var ErrBadInput = errors.New("bad input")

// NormalizeChannel lower-cases and adds the leading '#'.
func NormalizeChannel(c string) (string, error) {
	c = strings.ToLower(strings.TrimSpace(c))
	if c == "" {
		c = "#general"
	}
	if !strings.HasPrefix(c, "#") {
		c = "#" + c
	}
	if !channelName.MatchString(c) {
		return "", fmt.Errorf("%w: channel must look like #name (a-z, 0-9, _ or -)", ErrBadInput)
	}
	return c, nil
}

// Post appends a line and wakes every listener.
func (s *Store) Post(channel, nick, kind, text string) (Line, error) {
	return s.PostRef(channel, nick, kind, text, 0)
}

// PostRef is Post with a reference to another line (approvals).
func (s *Store) PostRef(channel, nick, kind, text string, ref int64) (Line, error) {
	channel, err := NormalizeChannel(channel)
	if err != nil {
		return Line{}, err
	}
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r", ""))
	if text == "" && kind != "join" && kind != "approve" && kind != "deny" {
		return Line{}, fmt.Errorf("%w: nothing to say", ErrBadInput)
	}
	if len([]rune(text)) > maxText {
		return Line{}, fmt.Errorf("%w: line is longer than %d characters; split it", ErrBadInput, maxText)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.lines[channel]; !ok && len(s.lines) >= maxChannels {
		return Line{}, fmt.Errorf("%w: too many channels", ErrBadInput)
	}
	l := Line{ID: s.nextID, Channel: channel, Nick: nick, Kind: kind, Text: text, Ref: ref, At: s.now().UTC()}
	raw, _ := json.Marshal(l)
	if _, err := s.file.Write(append(raw, '\n')); err != nil {
		return Line{}, err
	}
	s.remember(l)
	close(s.changed)
	s.changed = make(chan struct{})
	return l, nil
}

// Since returns lines in channel with ID > after, oldest first, at most max.
func (s *Store) Since(channel string, after int64, max int) []Line {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Line
	for _, l := range s.lines[channel] {
		if l.ID > after {
			out = append(out, l)
		}
	}
	if len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

// Wait blocks until a line newer than after exists in channel, or ctx ends.
func (s *Store) Wait(ctx context.Context, channel string, after int64, max int) []Line {
	for {
		s.mu.Lock()
		ch := s.changed
		s.mu.Unlock()
		if lines := s.Since(channel, after, max); len(lines) > 0 {
			return lines
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil
		}
	}
}

// Last returns the newest n lines of channel.
func (s *Store) Last(channel string, n int) []Line { return s.Since(channel, 0, n) }

// Topic returns the channel topic.
func (s *Store) Topic(channel string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.topics[channel]
}

// Seen returns each nick's last activity.
func (s *Store) Seen() map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]time.Time, len(s.seen))
	for k, v := range s.seen {
		out[k] = v
	}
	return out
}

// Touch records activity without posting (a listen counts as being here).
func (s *Store) Touch(nick string) {
	s.mu.Lock()
	s.seen[nick] = s.now().UTC()
	s.mu.Unlock()
}

// Find returns the line with id, in any channel still held in memory.
func (s *Store) Find(id int64) (Line, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, lines := range s.lines {
		for _, l := range lines {
			if l.ID == id {
				return l, true
			}
		}
	}
	return Line{}, false
}

// Close closes the log file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Close()
}

// Channels lists channels that have lines.
func (s *Store) Channels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.lines))
	for c := range s.lines {
		out = append(out, c)
	}
	return out
}
