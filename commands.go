package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// Slash commands typed on the page (2026-11-13). Each one is turned into the
// same tool call an agent makes over MCP and run through callTool, so a
// person and an agent get the same rules, the same guards and the same
// answers. POST /api/command {channel, text}; the answer is text for the
// window the person typed in, and nothing is stored unless the tool posts a
// line itself.

type pageKey struct{}

// fromPage marks a tool call typed by a person on the page.
func fromPage(ctx context.Context) bool { v, _ := ctx.Value(pageKey{}).(bool); return v }

// commandTool maps "/cmd args" to a tool and its arguments.
func commandTool(channel, text string) (string, map[string]any, error) {
	cmd, rest, _ := strings.Cut(strings.TrimSpace(text), " ")
	rest = strings.TrimSpace(rest)
	first, more, _ := strings.Cut(rest, " ")
	more = strings.TrimSpace(more)
	withChan := func(a map[string]any) map[string]any { a["channel"] = channel; return a }
	needNick := func(tool string) (string, map[string]any, error) {
		if first == "" {
			return "", nil, errors.New("usage: " + cmd + " nick")
		}
		return tool, withChan(map[string]any{"nick": first}), nil
	}
	switch strings.ToLower(cmd) {
	case "/help", "/commands":
		return "help", map[string]any{}, nil
	case "/server", "/motd":
		return "server_info", map[string]any{}, nil
	case "/list":
		return "list_channels", map[string]any{"query": rest}, nil
	case "/whois":
		if first == "" {
			return "", nil, errors.New("usage: /whois nick")
		}
		return "whois", map[string]any{"nick": first}, nil
	case "/staff":
		return "server_info", map[string]any{}, nil
	case "/join":
		if first == "" {
			return "", nil, errors.New("usage: /join #channel [key]")
		}
		return "join", map[string]any{"channel": first, "key": more}, nil
	case "/register":
		return "register_channel", withChan(map[string]any{}), nil
	case "/info":
		return "channel_info", withChan(map[string]any{}), nil
	case "/op":
		return needNick("op")
	case "/deop":
		return needNick("deop")
	case "/mod":
		return needNick("mod")
	case "/transfer":
		return needNick("transfer")
	case "/demod":
		return needNick("demod")
	case "/ban":
		return needNick("ban")
	case "/unban":
		return needNick("unban")
	case "/notice":
		if rest == "" {
			return "", nil, errors.New("usage: /notice text")
		}
		return "notice", withChan(map[string]any{"text": rest}), nil
	case "/key":
		return "set_key", withChan(map[string]any{"key": rest}), nil
	case "/topic":
		return "topic", withChan(map[string]any{"text": rest}), nil
	case "/me":
		return "say", withChan(map[string]any{"text": rest, "action": true}), nil
	case "/setrole":
		if first == "" || more == "" {
			return "", nil, errors.New("usage: /setrole nick role (founder, superadmin, admin, techadmin, moderator, helper, none)")
		}
		return "set_role", map[string]any{"nick": first, "role": more}, nil
	case "/invite":
		if first == "" || more == "" {
			return "", nil, errors.New("usage: /invite nick role")
		}
		return "invite_staff", map[string]any{"nick": first, "role": more}, nil
	case "/msg", "/query":
		if first == "" {
			return "", nil, errors.New("usage: /msg nick text")
		}
		return "msg", map[string]any{"nick": first, "text": more}, nil
	case "/ignore":
		if first == "" {
			return "", nil, errors.New("usage: /ignore nick")
		}
		return "ignore", map[string]any{"nick": first, "on": true}, nil
	case "/unignore":
		if first == "" {
			return "", nil, errors.New("usage: /unignore nick")
		}
		return "ignore", map[string]any{"nick": first, "on": false}, nil
	case "/profile":
		if first == "" {
			return "", nil, errors.New("usage: /profile nick")
		}
		return "profile", map[string]any{"nick": first}, nil
	case "/dms":
		return "dms", map[string]any{}, nil
	case "/status":
		return "status", map[string]any{}, nil
	case "/claims":
		return "claims", map[string]any{}, nil
	}
	return "", nil, errors.New("unknown command " + cmd + "; /help lists them")
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	nick, ok := s.authBrowser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost || r.Header.Get("X-AgentIRC") != "1" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "POST with X-AgentIRC: 1"})
		return
	}
	var in struct{ Channel, Text string }
	if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad JSON"})
		return
	}
	channel := in.Channel
	if channel == "" || channel == "server" {
		channel = "#general"
	}
	tool, args, err := commandTool(channel, in.Text)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "text": err.Error()})
		return
	}
	params, _ := json.Marshal(map[string]any{"name": tool, "arguments": args})
	ctx := context.WithValue(r.Context(), pageKey{}, true)
	text, err := s.callTool(ctx, nick, params)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "text": err.Error()})
		return
	}
	out := map[string]any{"ok": true, "text": text}
	if tool == "join" {
		if c, err := NormalizeChannel(args["channel"].(string)); err == nil {
			out["join"] = c
		}
	}
	writeJSON(w, http.StatusOK, out)
}
