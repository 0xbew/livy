// Package profile is `livy profile <sha|range>`: for each commit, where the time went in the
// agent sessions behind it, from the OTEL spans Livy's receiver stores and the ledger. Wall
// time from the first prompt to the commit splits into agent time (model calls and tools),
// time waiting on the human, and the rest; a span tree shows each turn's model calls, tools
// and subagents; rework signals come from the ledger and git. Text-free: it prints names,
// durations and counts, never prompt or tool text.
package profile

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/0xbew/livy/internal/otelspan"
)

// Kind is what a span stands for in a profile.
type Kind int

// The kinds a span can be. KindOther spans are kept in the tree's counts but not printed.
const (
	KindOther Kind = iota
	KindTurn       // one prompt's turn: the tree's root
	KindModel      // one model call
	KindTool       // one tool call (a subagent is an Agent or Task tool call)
	KindWait       // a tool call waiting on the human's approval
)

// Span is one stored OTEL span, with only its string and boolean attributes (a boolean as
// "true" or "false").
type Span otelspan.Span

// Kind is what the span stands for, by Claude Code's and Codex's span names.
func (s Span) Kind() Kind {
	switch {
	case s.Name == "claude_code.interaction" || s.Name == "session_task.turn":
		return KindTurn
	case s.Name == "claude_code.llm_request" || s.Attrs["codex.turn.phase"] == "sampling":
		return KindModel
	case s.Name == "claude_code.tool" || s.Attrs["codex.tool.phase"] == "dispatch":
		return KindTool
	case s.Name == "claude_code.tool.blocked_on_user":
		return KindWait
	}
	return KindOther
}

// sessionKeys are the attributes that name a span's session: Claude Code puts session.id on
// every span; Codex names its conversation on the turn and a few others only.
var sessionKeys = []string{"session.id", "thread_id", "conversation.id", "thread.id"}

// names says whether the span names session id.
func (s Span) names(id string) bool {
	for _, k := range sessionKeys {
		if v := s.Attrs[k]; v != "" && v == id {
			return true
		}
	}
	return false
}

// subagent says whether a tool call starts a subagent.
func (s Span) subagent() bool {
	t := s.Attrs["tool_name"]
	return t == "Agent" || t == "Task"
}

// Label is the span's name as a profile prints it: the model, the tool, or "subagent".
func (s Span) Label() string {
	switch s.Kind() {
	case KindTurn:
		return "turn"
	case KindModel:
		return "model " + orUnknown(s.Attrs["model"])
	case KindTool:
		if s.subagent() {
			return "subagent (" + s.Attrs["tool_name"] + ")"
		}
		return "tool " + orUnknown(s.Attrs["tool_name"])
	case KindWait:
		return "waiting on approval"
	}
	return s.Name
}

func orUnknown(v string) string {
	if v == "" {
		return "(unnamed)"
	}
	return v
}

// ReadSpans reads the trace spans in r, one otel stream line each; other signals' lines are
// skipped. bad counts the lines that could not be read.
func ReadSpans(r io.Reader) (spans []Span, bad int, err error) {
	got, bad, err := otelspan.Read(r)
	spans = make([]Span, 0, len(got))
	for _, s := range got {
		spans = append(spans, Span(s))
	}
	return spans, bad, err
}

// ReadSpansFile reads one otel stream file, gzipped when its name ends in .gz.
func ReadSpansFile(path string) (spans []Span, bad int, err error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = fh.Close() }() // opened read-only: a close error loses nothing
	var r io.Reader = fh
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(fh)
		if err != nil {
			return nil, 0, fmt.Errorf("gunzip %s: %w", path, err)
		}
		r = gz
	}
	return ReadSpans(r)
}
