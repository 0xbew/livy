// Package profile is `livy profile <sha|range>`: for each commit, where the time went in the
// agent sessions behind it, from the OTEL spans Livy's receiver stores and the ledger. Wall
// time from the first prompt to the commit splits into agent time (model calls and tools),
// time waiting on the human, and the rest; a span tree shows each turn's model calls, tools
// and subagents; rework signals come from the ledger and git. Text-free: it prints names,
// durations and counts, never prompt or tool text.
package profile

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxLine is the longest otel stream line read: one export can hold many spans.
const maxLine = 64 << 20

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
type Span struct {
	Trace, ID, Parent string
	Name              string
	Start, End        time.Time
	Attrs             map[string]string
}

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

// otlpLine is one line of the otel stream: one export as the receiver stored it.
type otlpLine struct {
	Signal string `json:"signal"`
	OTLP   struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []otlpSpan `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	} `json:"otlp"`
}

type otlpSpan struct {
	TraceID      string     `json:"traceId"`
	SpanID       string     `json:"spanId"`
	ParentSpanID string     `json:"parentSpanId"`
	Name         string     `json:"name"`
	Start        nanos      `json:"startTimeUnixNano"`
	End          nanos      `json:"endTimeUnixNano"`
	Attributes   []otlpAttr `json:"attributes"`
}

type otlpAttr struct {
	Key   string `json:"key"`
	Value struct {
		String *string `json:"stringValue"`
		Bool   *bool   `json:"boolValue"`
	} `json:"value"`
}

// nanos is an OTLP/JSON time: a decimal string of nanoseconds since the epoch (or a number).
type nanos int64

func (n *nanos) UnmarshalJSON(b []byte) error {
	v, err := strconv.ParseInt(strings.Trim(string(b), `"`), 10, 64)
	if err != nil {
		return fmt.Errorf("unix nanoseconds %s: %w", b, err)
	}
	*n = nanos(v)
	return nil
}

// ReadSpans reads the trace spans in r, one otel stream line each; other signals' lines are
// skipped. bad counts the lines that could not be read.
func ReadSpans(r io.Reader) (spans []Span, bad int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		var l otlpLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			bad++
			continue
		}
		if l.Signal != "traces" {
			continue
		}
		for _, rs := range l.OTLP.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					spans = append(spans, s.span())
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return spans, bad, fmt.Errorf("read otel stream: %w", err)
	}
	return spans, bad, nil
}

func (s otlpSpan) span() Span {
	attrs := make(map[string]string, len(s.Attributes))
	for _, a := range s.Attributes {
		switch {
		case a.Value.String != nil:
			attrs[a.Key] = *a.Value.String
		case a.Value.Bool != nil:
			attrs[a.Key] = strconv.FormatBool(*a.Value.Bool)
		}
	}
	return Span{Trace: s.TraceID, ID: s.SpanID, Parent: s.ParentSpanID, Name: s.Name,
		Start: time.Unix(0, int64(s.Start)).UTC(), End: time.Unix(0, int64(s.End)).UTC(), Attrs: attrs}
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
