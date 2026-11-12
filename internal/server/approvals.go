package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Approvals: an agent asks the owner a yes/no question as a structured
// record instead of a chat line ("can I deploy?"), and only a nick with the
// owner role can answer it. Both the ask and the answer are lines in the
// channel, so the decision is logged next to the question it decides.
//
// Kinds: "ask" (Line.ID is the ask's id), "approve" and "deny" (Line.Ref
// points at the ask).

var approvalTools = []map[string]any{
	{"name": "ask", "description": "Ask the owner for a yes/no decision. Returns the ask id; wait for the decision with await_answer.",
		"inputSchema": schema(map[string]any{
			"channel":  channelProp,
			"question": map[string]any{"type": "string", "description": "One question the owner can answer yes or no."},
		}, "question")},
	{"name": "answer", "description": "Owner only: approve or deny an ask by its id, with an optional note.",
		"inputSchema": schema(map[string]any{
			"id":       map[string]any{"type": "integer"},
			"decision": map[string]any{"type": "string", "enum": []string{"approve", "deny"}},
			"note":     map[string]any{"type": "string"},
		}, "id", "decision")},
	{"name": "await_answer", "description": "Wait up to wait_seconds (max 50) for the owner's decision on an ask.",
		"inputSchema": schema(map[string]any{
			"id":           map[string]any{"type": "integer"},
			"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 50},
		}, "id")},
}

func init() { tools = append(tools, approvalTools...) }

// writeTools are refused for guests. Read tools (listen, history, who,
// claims, await_answer, topic without text) stay open.
var writeTools = map[string]bool{"say": true, "ask": true, "answer": true, "claim": true, "release": true}

// guardWrite refuses a write by a guest. topic is a write only with text.
func (s *Server) guardWrite(nick, tool string, params json.RawMessage) error {
	if s.tokens.Role(nick) != RoleGuest {
		return nil
	}
	write := writeTools[tool]
	if tool == "topic" {
		var p struct {
			Arguments struct {
				Text string `json:"text"`
			} `json:"arguments"`
		}
		_ = json.Unmarshal(params, &p)
		write = strings.TrimSpace(p.Arguments.Text) != ""
	}
	if write {
		return errors.New("read-only guest: this nick can listen but not write")
	}
	return nil
}

// callApprovalTool answers ask, answer and await_answer; handled is false
// for any other tool.
func (s *Server) callApprovalTool(ctx context.Context, nick, name string, params json.RawMessage) (string, bool, error) {
	if name != "ask" && name != "answer" && name != "await_answer" {
		return "", false, nil
	}
	var p struct {
		Arguments struct {
			Channel     string `json:"channel"`
			Question    string `json:"question"`
			ID          askRef `json:"id"`
			Decision    string `json:"decision"`
			Note        string `json:"note"`
			WaitSeconds int    `json:"wait_seconds"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", true, errors.New("bad arguments")
	}
	a := p.Arguments
	switch name {
	case "ask":
		channel, err := NormalizeChannel(a.Channel)
		if err != nil {
			return "", true, err
		}
		if !s.limit.allow(nick) {
			return "", true, errors.New("slow down: too many lines; wait a few seconds")
		}
		s.ensureJoined(channel, nick)
		l, err := s.store.PostRef(channel, nick, "ask", a.Question, 0)
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("%s\n(ask #%d is waiting for the owner; use await_answer)", l, l.ID), true, nil
	case "answer":
		if s.tokens.Role(nick) != RoleOwner {
			return "", true, errors.New("only the owner can answer an ask")
		}
		l, err := s.decide(nick, int64(a.ID), a.Decision, a.Note)
		if err != nil {
			return "", true, err
		}
		return l.String(), true, nil
	default: // await_answer
		ask, ok := s.store.Find(int64(a.ID))
		if !ok || ask.Kind != "ask" {
			return "", true, fmt.Errorf("no ask #%d", int64(a.ID))
		}
		if l, ok := s.decisionFor(ask); ok {
			return l.String(), true, nil
		}
		wait := min(max(a.WaitSeconds, 0), 50)
		wctx, cancel := context.WithTimeout(ctx, time.Duration(wait)*time.Second)
		defer cancel()
		after := ask.ID
		for {
			lines := s.store.Wait(wctx, ask.Channel, after, 100)
			if len(lines) == 0 {
				return fmt.Sprintf("(no decision on ask #%d yet)", ask.ID), true, nil
			}
			for _, l := range lines {
				if l.Ref == ask.ID && (l.Kind == "approve" || l.Kind == "deny") {
					return l.String(), true, nil
				}
			}
			after = lines[len(lines)-1].ID
		}
	}
}

// decide records the owner's decision on ask id. An ask is decided once.
func (s *Server) decide(owner string, id int64, decision, note string) (Line, error) {
	if decision != "approve" && decision != "deny" {
		return Line{}, errors.New("decision must be approve or deny")
	}
	ask, ok := s.store.Find(id)
	if !ok || ask.Kind != "ask" {
		return Line{}, fmt.Errorf("no ask #%d", id)
	}
	if prev, done := s.decisionFor(ask); done {
		return Line{}, fmt.Errorf("ask #%d was already decided: %s", id, prev)
	}
	return s.store.PostRef(ask.Channel, owner, decision, note, ask.ID)
}

// decisionFor returns the line that decided ask, if any.
func (s *Server) decisionFor(ask Line) (Line, bool) {
	for _, l := range s.store.Since(ask.Channel, ask.ID, KeepInRAM) {
		if l.Ref == ask.ID && (l.Kind == "approve" || l.Kind == "deny") {
			return l, true
		}
	}
	return Line{}, false
}

// askRef is an ask id that also accepts "12" and "#12": models often send
// ids as strings, and refusing those as "bad arguments" helps nobody.
type askRef int64

func (r *askRef) UnmarshalJSON(b []byte) error {
	s := strings.TrimPrefix(strings.Trim(string(b), `"`), "#")
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return fmt.Errorf("id must be a number, got %s", b)
	}
	*r = askRef(n)
	return nil
}
