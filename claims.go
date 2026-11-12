package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Claims: a session says which paths it is about to edit, and the server
// refuses a claim that overlaps one another nick holds. "I'll edit X" as a
// chat line was check-then-act: two sessions could read the same quiet
// history, both post, and both go ahead. The server sees every call in order,
// so here only one of them gets a yes (marginalia_claude and hobosentinel on
// Moltbook, 2026-11-12).
//
// A claim is a lease: it ends at its expiry even if the session that took it
// died mid-edit, so a dead session cannot block the others for good.
//
// Claims are per server (one server is one project), not per channel. Each
// claim and release is also said in the channel as a /me line, so people who
// only read the room see it.

const (
	defaultClaimTTL = 30 * time.Minute
	maxClaimTTL     = 4 * time.Hour
	maxClaimPaths   = 50
	maxClaimsHeld   = 500
	maxClaimNote    = 200
)

type claim struct {
	Path    string    `json:"path"`
	Nick    string    `json:"nick"`
	Note    string    `json:"note,omitempty"`
	Expires time.Time `json:"expires"`
}

type claimBook struct {
	mu    sync.Mutex
	file  string
	items []claim
	now   func() time.Time
}

// errClaimRefused is returned when a path overlaps another nick's claim.
var errClaimRefused = errors.New("claim refused")

// openClaims loads dir/claims.json. A missing or unreadable file starts empty:
// every claim is a lease, so losing them costs at most one expiry period.
func openClaims(dir string) *claimBook {
	b := &claimBook{file: filepath.Join(dir, "claims.json"), now: time.Now}
	if raw, err := os.ReadFile(b.file); err == nil {
		_ = json.Unmarshal(raw, &b.items)
	}
	return b
}

// normalizeClaimPath turns what a session typed into a project-relative,
// slash-separated path. A segment with a glob character claims its directory
// instead ("mobile/lib/l10n/*.arb" claims "mobile/lib/l10n"): over-claiming
// costs a wait, under-claiming costs a collision. "" or "." is the whole tree.
func normalizeClaimPath(p string) (string, error) {
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	if p == "" {
		return "", fmt.Errorf("%w: empty path", ErrBadInput)
	}
	segs := strings.Split(p, "/")
	var keep []string
	for _, s := range segs {
		if s == ".." {
			return "", fmt.Errorf("%w: %q leaves the project (..)", ErrBadInput, p)
		}
		if strings.ContainsAny(s, "*?[{") {
			break
		}
		keep = append(keep, s)
	}
	out := strings.TrimPrefix(path.Clean("/"+strings.Join(keep, "/")), "/")
	if out == "" {
		out = "."
	}
	return out, nil
}

// claimsOverlap: the same path, or one is a directory above the other.
// Compared without case, since a Windows checkout does not tell them apart.
func claimsOverlap(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	return a == b || a == "." || b == "." ||
		strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

func (b *claimBook) pruneLocked() {
	now := b.now()
	kept := b.items[:0]
	for _, c := range b.items {
		if c.Expires.After(now) {
			kept = append(kept, c)
		}
	}
	b.items = kept
}

func (b *claimBook) saveLocked() error {
	raw, err := json.MarshalIndent(b.items, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.file)
}

// Claim takes every path for nick, or none of them. A path the nick already
// holds is renewed with the new expiry and note.
func (b *claimBook) Claim(nick string, paths []string, ttl time.Duration, note string) ([]claim, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("%w: name at least one path", ErrBadInput)
	}
	if len(paths) > maxClaimPaths {
		return nil, fmt.Errorf("%w: at most %d paths per claim", ErrBadInput, maxClaimPaths)
	}
	if ttl <= 0 {
		ttl = defaultClaimTTL
	}
	ttl = min(ttl, maxClaimTTL)
	note = strings.TrimSpace(note)
	if len([]rune(note)) > maxClaimNote {
		return nil, fmt.Errorf("%w: note is longer than %d characters", ErrBadInput, maxClaimNote)
	}
	var want []string
	seen := map[string]bool{}
	for _, p := range paths {
		n, err := normalizeClaimPath(p)
		if err != nil {
			return nil, err
		}
		if !seen[strings.ToLower(n)] {
			seen[strings.ToLower(n)] = true
			want = append(want, n)
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked()
	var conflicts []string
	for _, w := range want {
		for _, c := range b.items {
			if c.Nick != nick && claimsOverlap(w, c.Path) {
				msg := fmt.Sprintf("%s overlaps %s, held by %s until %s", w, c.Path, c.Nick, clock(c.Expires))
				if c.Note != "" {
					msg += " (" + c.Note + ")"
				}
				conflicts = append(conflicts, msg)
			}
		}
	}
	if len(conflicts) > 0 {
		return nil, fmt.Errorf("%w, nothing was claimed: %s. Wait for the expiry, or ask the holder to release",
			errClaimRefused, strings.Join(conflicts, "; "))
	}
	renewed := func(c claim) bool { return c.Nick == nick && seen[strings.ToLower(c.Path)] }
	others := 0
	for _, c := range b.items {
		if !renewed(c) {
			others++
		}
	}
	if others+len(want) > maxClaimsHeld {
		return nil, fmt.Errorf("%w: too many claims are held; release some first", ErrBadInput)
	}
	expires := b.now().Add(ttl).UTC()
	kept := b.items[:0]
	for _, c := range b.items {
		if !renewed(c) {
			kept = append(kept, c)
		}
	}
	b.items = kept
	out := make([]claim, 0, len(want))
	for _, w := range want {
		c := claim{Path: w, Nick: nick, Note: note, Expires: expires}
		b.items = append(b.items, c)
		out = append(out, c)
	}
	if err := b.saveLocked(); err != nil {
		return nil, err
	}
	return out, nil
}

// Release drops nick's claims on the given paths, or all of nick's claims
// when paths is empty. A nick can only release its own claims. It returns the
// paths released.
func (b *claimBook) Release(nick string, paths []string) ([]string, error) {
	drop := map[string]bool{}
	for _, p := range paths {
		n, err := normalizeClaimPath(p)
		if err != nil {
			return nil, err
		}
		drop[strings.ToLower(n)] = true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked()
	var gone []string
	kept := b.items[:0]
	for _, c := range b.items {
		if c.Nick == nick && (len(paths) == 0 || drop[strings.ToLower(c.Path)]) {
			gone = append(gone, c.Path)
			continue
		}
		kept = append(kept, c)
	}
	b.items = kept
	if len(gone) > 0 {
		if err := b.saveLocked(); err != nil {
			return nil, err
		}
	}
	return gone, nil
}

// List returns the live claims, soonest expiry first.
func (b *claimBook) List() []claim {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked()
	out := append([]claim(nil), b.items...)
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Expires.Equal(out[j].Expires) {
			return out[i].Expires.Before(out[j].Expires)
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func clock(t time.Time) string { return t.Local().Format("15:04") }

var claimTools = []map[string]any{
	{"name": "claim", "description": "Claim paths you are about to edit, as a lease. Refused, with nothing claimed, if any path is the same as, inside or above a path another nick holds; the refusal names the holder and the expiry. A glob segment claims its directory. Claiming a path you already hold renews it. Release when done.",
		"inputSchema": schema(map[string]any{
			"paths":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Project-relative paths or directories, e.g. [\"server/auth\", \"README.md\"]."},
			"ttl_minutes": map[string]any{"type": "integer", "minimum": 1, "maximum": 240, "description": "How long the claim lasts (default 30)."},
			"note":        map[string]any{"type": "string", "description": "What you are doing, shown to anyone refused."},
			"channel":     channelProp,
		}, "paths")},
	{"name": "release", "description": "Release your claims on the given paths, or all of your claims when paths is empty.",
		"inputSchema": schema(map[string]any{
			"paths":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"channel": channelProp,
		})},
	{"name": "claims", "description": "List the live claims: path, holder, expiry and note.",
		"inputSchema": schema(map[string]any{})},
}

func init() { tools = append(tools, claimTools...) }

// callClaimTool answers claim, release and claims; handled is false for any
// other tool.
func (s *Server) callClaimTool(_ context.Context, nick, name string, params json.RawMessage) (text string, handled bool, err error) {
	if name != "claim" && name != "release" && name != "claims" {
		return "", false, nil
	}
	var p struct {
		Arguments struct {
			Paths      []string `json:"paths"`
			TTLMinutes int      `json:"ttl_minutes"`
			Note       string   `json:"note"`
			Channel    string   `json:"channel"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", true, errors.New("bad arguments")
	}
	a := p.Arguments
	s.store.Touch(nick)
	if name == "claims" {
		list := s.claims.List()
		if len(list) == 0 {
			return "No live claims.", true, nil
		}
		var sb strings.Builder
		sb.WriteString("Live claims:")
		for _, c := range list {
			fmt.Fprintf(&sb, "\n  %s  held by %s until %s", c.Path, c.Nick, clock(c.Expires))
			if c.Note != "" {
				sb.WriteString("  (" + c.Note + ")")
			}
		}
		return sb.String(), true, nil
	}
	channel, err := NormalizeChannel(a.Channel)
	if err != nil {
		return "", true, err
	}
	if !s.limit.allow(nick) {
		return "", true, errors.New("slow down: too many calls; wait a few seconds")
	}
	if name == "claim" {
		got, err := s.claims.Claim(nick, a.Paths, time.Duration(a.TTLMinutes)*time.Minute, a.Note)
		if err != nil {
			return "", true, err
		}
		paths := make([]string, len(got))
		for i, c := range got {
			paths[i] = c.Path
		}
		until := clock(got[0].Expires)
		line := fmt.Sprintf("claims %s until %s", strings.Join(paths, ", "), until)
		if got[0].Note != "" {
			line += ": " + got[0].Note
		}
		s.ensureJoined(channel, nick)
		if _, err := s.store.Post(channel, nick, "action", line); err != nil {
			return "", true, err
		}
		return fmt.Sprintf("Claimed %s until %s. Release when done.", strings.Join(paths, ", "), until), true, nil
	}
	gone, err := s.claims.Release(nick, a.Paths)
	if err != nil {
		return "", true, err
	}
	if len(gone) == 0 {
		return "You held none of those claims; nothing released.", true, nil
	}
	s.ensureJoined(channel, nick)
	if _, err := s.store.Post(channel, nick, "action", "released "+strings.Join(gone, ", ")); err != nil {
		return "", true, err
	}
	return "Released " + strings.Join(gone, ", ") + ".", true, nil
}
