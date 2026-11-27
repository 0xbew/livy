package profile

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/0xbew/livy/internal/promptcache"
	"github.com/0xbew/livy/internal/why"
)

// A window's trace: every span placed in its turns' trees, in tree order, with the agents that
// ran them and the skill loads and compactions between them. A model span carries the
// usage.api_call it joins to (by API message id, else request id): its tokens, the size of the
// context it sent, and how much of the prompt cache it reused. Names, ids, times and counts only.

// Trace span kinds, as `livy profile --json` writes them.
const (
	traceTurn     = "turn"
	traceModel    = "model"
	traceTool     = "tool"
	traceSubagent = "subagent"
	traceApproval = "approval"
)

// Marker kinds.
const (
	markerSkill      = "skill"
	markerCompaction = "compaction"
	eventInteraction = "interaction"
	kindCompacted    = "compacted"
)

// JSONTraceSpan is one span of a window's trace, clipped to the window. ID is unique within the
// window and Parent is its parent's ID ("" for a turn). Kind is "turn", "model", "tool",
// "subagent" (an Agent or Task call) or "approval" (a tool waiting on the human). Agent,
// StartsAgent and Outcome are as JSONSpan's. Call is a model span's call; null for any other
// span, and for a model span no usage.api_call line matches.
type JSONTraceSpan struct {
	ID          string    `json:"id"`
	Parent      string    `json:"parent"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	DurationMS  int64     `json:"duration_ms"`
	Agent       string    `json:"agent"`
	StartsAgent string    `json:"starts_agent"`
	Outcome     string    `json:"outcome"`
	Call        *JSONCall `json:"call"`
	// Target is a tool or subagent span's kept target (package tooltarget: a file, folder,
	// program, host, skill or task label); "" when none was kept on this machine.
	Target string `json:"target"`
}

// JSONCall is one model call's usage.api_call: its tokens, the context it sent (uncached input,
// cache reads and cache writes) and its cache verdict.
type JSONCall struct {
	Model         string    `json:"model"`
	MessageID     string    `json:"message_id"`
	RequestID     string    `json:"request_id"`
	InputTokens   int64     `json:"input_tokens"`
	OutputTokens  int64     `json:"output_tokens"`
	CacheRead     int64     `json:"cache_read_tokens"`
	CacheWrite    int64     `json:"cache_write_tokens"`
	ContextTokens int64     `json:"context_tokens"`
	Cache         JSONCache `json:"cache"`
}

// JSONCache is a call's cache verdict. Class is "first", "hit", "partial_miss", "full_miss" or
// "" (the call has no id to judge it by); Cause, for a miss only, is "compaction",
// "idle_past_5m", "idle_past_1h", "model_switch" or "unexplained". PreviousPrefix is the cache
// reads plus writes of the previous call on its cache, TokensRewritten the part of that prefix a
// miss wrote again, and GapMS the time since that call (null for a first call).
type JSONCache struct {
	Class           string `json:"class"`
	Cause           string `json:"cause"`
	PreviousPrefix  int64  `json:"previous_prefix"`
	TokensRewritten int64  `json:"tokens_rewritten"`
	GapMS           *int64 `json:"gap_ms"`
	MidBurstDrop    bool   `json:"mid_burst_drop"`
}

// JSONAgent is one lane of a window's trace: "main" (the session's own calls) or a subagent, its
// type when the ledger names it, the span that launched it and the agent that span belongs to
// ("" for main).
type JSONAgent struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// Label is the subagent's task label: the target kept for the Agent call that ran it.
	Label      string `json:"label"`
	LaunchedBy string `json:"launched_by"`
	Parent     string `json:"parent"`
}

// JSONMarker is a moment in a window: a skill use (Name is the skill, Method how it was used, as
// livy.pr/1's skills say it) or a compaction.
type JSONMarker struct {
	Kind   string    `json:"kind"`
	At     time.Time `json:"ts"`
	Agent  string    `json:"agent"`
	Name   string    `json:"name"`
	Method string    `json:"method"`
}

// calls is the usage.api_call lines a model span joins to, and every call's cache verdict.
type calls struct {
	byMessage, byRequest map[string]why.Event
	verdicts             map[promptcache.Key]promptcache.Verdict
}

func callsOf(events []why.Event) calls {
	c := calls{byMessage: map[string]why.Event{}, byRequest: map[string]why.Event{}, verdicts: promptcache.Classify(events)}
	for _, e := range events {
		if e.Event != eventAPICall {
			continue
		}
		if e.MessageID != "" {
			c.byMessage[e.MessageID] = e
		}
		if e.RequestID != "" {
			c.byRequest[e.RequestID] = e
		}
	}
	return c
}

// call is a model span's call, nil when no line matches it.
func (c calls) call(s Span) *JSONCall {
	e, ok := c.byMessage[s.Attrs["gen_ai.response.id"]]
	if !ok || s.Attrs["gen_ai.response.id"] == "" {
		e, ok = c.byRequest[s.Attrs["request_id"]]
		if !ok || s.Attrs["request_id"] == "" {
			return nil
		}
	}
	v := c.verdicts[promptcache.Key{Message: e.MessageID, Request: e.RequestID}]
	out := &JSONCall{Model: e.Model, MessageID: e.MessageID, RequestID: e.RequestID, InputTokens: e.InputTokens,
		OutputTokens: e.OutputTokens, CacheRead: e.CacheReadTokens, CacheWrite: e.CacheCreationTokens,
		ContextTokens: e.InputTokens + e.CacheReadTokens + e.CacheCreationTokens,
		Cache: JSONCache{Class: string(v.Class), Cause: string(v.Cause), PreviousPrefix: v.Prefix,
			TokensRewritten: v.Rewritten(), MidBurstDrop: v.MidBurstDrop()}}
	if v.Class != promptcache.ClassFirst && v.Class != promptcache.ClassUnknown {
		gap := v.Gap.Milliseconds()
		out.Cache.GapMS = &gap
	}
	return out
}

// traceOf is p's trace: its spans, the agents that ran them, and the window's markers.
func (b *jsonBuilder) traceOf(p Profile, session string) (spans []JSONTraceSpan, agents []JSONAgent, markers []JSONMarker) {
	t := tracer{b: b, agents: []JSONAgent{{ID: mainAgent}}, seen: map[string]bool{mainAgent: true}}
	for _, n := range p.Turns {
		t.walk(n, "")
	}
	return t.spans, t.agents, b.markersIn(session, p.Start, p.End)
}

// tracer numbers a window's spans as it walks their trees.
type tracer struct {
	b      *jsonBuilder
	spans  []JSONTraceSpan
	agents []JSONAgent
	seen   map[string]bool
}

func (t *tracer) walk(n *Node, parent string) {
	id := fmt.Sprintf("s%d", len(t.spans)+1)
	kind, name := traceKindName(n.Span)
	s := JSONTraceSpan{ID: id, Parent: parent, Kind: kind, Name: name, Start: n.Start.UTC(), End: n.End.UTC(),
		DurationMS: n.Duration().Milliseconds(), Agent: cmp.Or(n.Span.Attrs["agent_id"], mainAgent), Outcome: string(n.Outcome)}
	if kind == traceModel {
		s.Call = t.b.calls.call(n.Span)
	}
	if kind == traceTool || kind == traceSubagent {
		s.Target = t.b.targets[n.Span.Attrs["tool_use_id"]]
	}
	t.agent(s.Agent, "", "", "")
	if kind == traceSubagent {
		s.StartsAgent = startedAgent(n)
		if s.StartsAgent != "" {
			t.agent(s.StartsAgent, id, s.Agent, s.Target)
		}
	}
	t.spans = append(t.spans, s)
	for _, c := range n.Children {
		t.walk(c, id)
	}
}

// agent adds a lane the first time an agent is seen; a later launch fills in who launched it
// and its label.
func (t *tracer) agent(id, launchedBy, parent, label string) {
	if !t.seen[id] {
		t.seen[id] = true
		t.agents = append(t.agents, JSONAgent{ID: id, Type: t.b.agentTypes[id]})
	}
	if launchedBy == "" {
		return
	}
	i := slices.IndexFunc(t.agents, func(a JSONAgent) bool { return a.ID == id })
	if t.agents[i].LaunchedBy == "" {
		t.agents[i].LaunchedBy, t.agents[i].Parent, t.agents[i].Label = launchedBy, parent, label
	}
}

// traceKindName is a span's trace kind and the model or tool it names.
func traceKindName(s Span) (kind, name string) {
	switch s.Kind() {
	case KindTurn:
		return traceTurn, ""
	case KindWait:
		return traceApproval, ""
	case KindModel, KindTool, KindOther:
	}
	k, name := spanKindName(s)
	switch k {
	case "model":
		return traceModel, name
	case "subagent":
		return traceSubagent, name
	}
	return traceTool, name
}

// markersOf is every session's skill uses (each once) and compactions, oldest first.
func markersOf(events []why.Event) map[string][]JSONMarker {
	out := map[string][]JSONMarker{}
	add := func(e why.Event, m JSONMarker) {
		if at, ok := instant(e.TS); ok {
			m.At, m.Agent = at.UTC(), cmp.Or(e.AgentID, mainAgent)
			out[e.SessionID] = append(out[e.SessionID], m)
		}
	}
	for _, e := range why.SkillLoads(events) {
		add(e, JSONMarker{Kind: markerSkill, Name: e.Skill, Method: why.SkillMethod(e.Trigger)})
	}
	for _, e := range events {
		if e.Event == eventInteraction && e.Kind == kindCompacted {
			add(e, JSONMarker{Kind: markerCompaction})
		}
	}
	for _, ms := range out {
		slices.SortStableFunc(ms, func(a, b JSONMarker) int { return a.At.Compare(b.At) })
	}
	return out
}

// markersIn is session's markers from start to end.
func (b *jsonBuilder) markersIn(session string, start, end time.Time) []JSONMarker {
	out := []JSONMarker{}
	for _, m := range b.markers[session] {
		if !m.At.Before(start) && !m.At.After(end) {
			out = append(out, m)
		}
	}
	return out
}

// SessionsOf is the sessions behind the commits, to read their tool targets with.
func SessionsOf(commits []why.Explained) map[string]bool {
	out := map[string]bool{}
	for _, c := range commits {
		for _, s := range c.Report.Sessions {
			out[s.ID] = true
		}
	}
	return out
}
