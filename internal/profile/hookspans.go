package profile

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/0xbew/livy/internal/why"
)

// Source is where a profile's times came from.
type Source string

// The sources a window can be timed from. Spans win when they time the window; Livy's own
// ledger lines stand in when no span reaches Livy, as on a machine whose managed settings send
// Claude Code's OTEL elsewhere.
const (
	SourceNone  Source = ""
	SourceOTEL  Source = "otel"
	SourceHooks Source = "hooks"
)

// Timing from the ledger. A turn runs from its prompt.submitted to its turn.ended (or the main
// agent's last call before the next prompt). A tool call runs over its tool.call line's
// start_ts..end_ts (the PostToolUse hook's duration, or the transcript backfill's), else from
// the call that asked for it to its tool.outcome. A model call ends at its usage.api_call and
// began at the last thing its agent was waiting on: the prompt, its previous call, the tools
// that call asked for, or, for a subagent's first call, the Agent call that ran it. A call with
// none of these is left out, so its time shows as unknown rather than as a guess.
//
// A session with no prompt line (Claude Code ran outside every in-scope repo, so neither its
// hooks nor extraction wrote one, while its calls worked in one) has its turns inferred: each
// runs from its first main-agent call's end to the last call or tool before the next turn's
// first call, and a turn ends at a call whose call.detail says it handed back (end_turn). The
// first call of each turn has no start, so its time falls in the wait before the turn.

// Span names Kind reads, and the attributes a model call carries, as Claude Code names its own
// (gen_ai.response.id is the API message id), so a reader joins either kind to usage.api_call.
const (
	nameTurn  = "claude_code.interaction"
	nameModel = "claude_code.llm_request"
	nameTool  = "claude_code.tool"
)

// Ledger events timing reads.
const (
	eventPrompt  = "prompt.submitted"
	eventStop    = "turn.ended"
	eventAPICall = "usage.api_call"
	eventTool    = "tool.call"
	eventOutcome = "tool.outcome"
	eventDetail  = "call.detail"
	sourceHook   = "hook"
	// stopHandBack is a call.detail stop_reason that ends the turn.
	stopHandBack = "end_turn"
)

// profileOf is session s's profile in its window: from the spans when they time it, else from
// the session's ledger lines (TurnsInferred when it has no prompt line). Codex and Amp sessions
// are timed from spans only.
func profileOf(spans []Span, events []why.Event, s why.Session) (Profile, Source) {
	w := windowOf(s)
	p := Build(spans, w)
	if p.Timed {
		return p, SourceOTEL
	}
	if s.Harness == "codex" || s.Harness == "amp" {
		return p, SourceNone
	}
	spans, inferred := hookSpans(events, s.ID)
	hp := Build(spans, w)
	if !hp.Timed {
		return p, SourceNone
	}
	hp.TurnsInferred = inferred
	return hp, SourceHooks
}

// toolTime is one tool call as the ledger times it.
type toolTime struct {
	id, tool, agent, spawned, result string
	start, end                       time.Time
	fromHook                         bool
}

// callTime is one model call as the ledger times it: when it ended.
type callTime struct {
	agent, model, message, request string
	end                            time.Time
}

// sessionLines are one session's ledger lines that time it.
type sessionLines struct {
	prompts, stops []time.Time
	calls          []callTime
	tools          map[string]*toolTime
	outcomes       []toolTime
	// launched is, per API message, the tools its call.detail says it asked for; handBack the
	// messages whose call.detail says the model handed back.
	launched map[string][]string
	handBack map[string]bool
}

// hookSpans are the spans session id's ledger lines stand for, named as Claude Code names its
// own, under one trace of their own; inferred is true when the session has no prompt line.
func hookSpans(events []why.Event, id string) (spans []Span, inferred bool) {
	l := readSessionLines(events, id)
	inferred = len(l.prompts) == 0
	tools := l.toolTimes()
	b := spanBuilder{session: id, trace: "livy-hooks:" + id, launcher: map[string]string{}}
	launches := map[string]time.Time{}
	for _, t := range tools {
		if t.spawned != "" {
			launches[t.spawned] = t.start
		}
	}
	calls := placeCalls(l, tools, launches)
	l.calls = make([]callTime, 0, len(calls))
	for _, c := range calls {
		l.calls = append(l.calls, c.callTime)
	}
	turns := b.turns(l, tools)
	if inferred {
		turns = b.inferredTurns(l, tools)
	}
	for _, t := range tools {
		sid := b.tool(t, b.parentOf(turns, t.agent, t.start))
		if t.spawned != "" {
			b.launcher[t.spawned] = sid
		}
	}
	for _, c := range calls {
		if !c.placed || !c.end.After(c.start) {
			continue
		}
		b.add(nameModel, c.start, c.end, b.parentOf(turns, c.agent, c.start), map[string]string{"model": c.model,
			"agent_id": c.agent, "gen_ai.response.id": c.message, "request_id": c.request})
	}
	return b.spans, inferred
}

// readSessionLines picks session id's timing lines out of events. A tool call two lines describe
// (its hook and the transcript backfill) is the hook's.
func readSessionLines(events []why.Event, id string) sessionLines {
	l := sessionLines{tools: map[string]*toolTime{}, launched: map[string][]string{}, handBack: map[string]bool{}}
	seenCall := map[string]bool{}
	for _, e := range events {
		if e.SessionID != id {
			continue
		}
		at, ok := instant(e.TS)
		if !ok {
			continue
		}
		switch e.Event {
		case eventPrompt:
			l.prompts = append(l.prompts, at)
		case eventStop:
			l.stops = append(l.stops, at)
		case eventAPICall:
			if e.MessageID != "" && seenCall[e.MessageID] {
				continue
			}
			seenCall[e.MessageID] = true
			l.calls = append(l.calls, callTime{agent: e.AgentID, model: e.Model, message: e.MessageID, request: e.RequestID, end: at})
		case eventTool:
			l.addToolCall(e)
		case eventDetail:
			if e.MessageID != "" && e.Tools != nil {
				l.launched[e.MessageID] = e.Tools
			}
			if e.MessageID != "" && e.StopReason == stopHandBack {
				l.handBack[e.MessageID] = true
			}
		case eventOutcome:
			l.outcomes = append(l.outcomes, toolTime{id: e.ToolUseID, tool: e.Tool, agent: e.AgentID, end: at,
				result: outcomeResult(e.Outcome)})
		}
	}
	slices.SortFunc(l.prompts, time.Time.Compare)
	slices.SortFunc(l.stops, time.Time.Compare)
	slices.SortStableFunc(l.calls, func(a, b callTime) int { return a.end.Compare(b.end) })
	return l
}

// addToolCall keeps a tool.call line that says when the call ended, the hook's over the backfill's.
func (l *sessionLines) addToolCall(e why.Event) {
	end, ok := instant(e.EndTS)
	if !ok || e.ToolUseID == "" {
		return
	}
	hook := e.Source == sourceHook
	if old := l.tools[e.ToolUseID]; old != nil && (old.fromHook || !hook) {
		return
	}
	t := &toolTime{id: e.ToolUseID, tool: e.Tool, agent: e.AgentID, spawned: e.SpawnedAgentID, end: end,
		result: e.Result, fromHook: hook}
	if start, ok := instant(e.StartTS); ok && !start.After(end) {
		t.start = start
	}
	l.tools[e.ToolUseID] = t
}

// outcomeResult is a tool.outcome's outcome as tool.call's result: ran is ok; a call refused or
// never run is left unsaid.
func outcomeResult(outcome string) string {
	if outcome == "ran" {
		return "ok"
	}
	return ""
}

// toolTimes are the session's tool calls, by start: each tool.call line, and each tool.outcome
// no tool.call line covers. A call with no start began when its agent's last model call before
// its end did; one with no such call is left out.
func (l sessionLines) toolTimes() []toolTime {
	var out []toolTime
	add := func(t toolTime) {
		if t.start.IsZero() {
			from, ok := lastCallEnd(l.calls, t.agent, t.end)
			if !ok {
				return
			}
			t.start = from
		}
		out = append(out, t)
	}
	for _, t := range l.tools {
		add(*t)
	}
	for _, t := range l.outcomes {
		if l.tools[t.id] == nil {
			add(t)
		}
	}
	slices.SortStableFunc(out, func(a, b toolTime) int { return cmp.Or(a.start.Compare(b.start), cmp.Compare(a.id, b.id)) })
	return out
}

// lastCallEnd is when agent's last model call that ended by at ended.
func lastCallEnd(calls []callTime, agent string, at time.Time) (time.Time, bool) {
	var last time.Time
	for _, c := range calls {
		if c.agent == agent && !c.end.After(at) {
			last = c.end
		}
	}
	return last, !last.IsZero()
}

// spanBuilder numbers the spans it makes within one trace.
type spanBuilder struct {
	session, trace string
	spans          []Span
	// launcher is the span id of the Agent call that ran each subagent.
	launcher map[string]string
}

// add appends one span and returns its id.
func (b *spanBuilder) add(name string, start, end time.Time, parent string, attrs map[string]string) string {
	id := fmt.Sprintf("h%d", len(b.spans)+1)
	a := map[string]string{"session.id": b.session}
	for k, v := range attrs {
		if v != "" {
			a[k] = v
		}
	}
	b.spans = append(b.spans, Span{Trace: b.trace, ID: id, Parent: parent, Name: name, Start: start, End: end, Attrs: a})
	return id
}

// tool adds one tool call's span, with an execution below it saying how it ended, and returns
// its id.
func (b *spanBuilder) tool(t toolTime, parent string) string {
	id := b.add(nameTool, t.start, t.end, parent, map[string]string{"tool_name": t.tool, "agent_id": t.agent, "tool_use_id": t.id})
	if ok := map[string]string{"ok": "true", "error": "false"}[t.result]; ok != "" {
		b.add(toolExecution, t.start, t.end, id, map[string]string{"success": ok})
	}
	return id
}

// turnSpan is one turn's span id and times.
type turnSpan struct {
	id         string
	start, end time.Time
}

// turns adds a span per prompt, to its turn.ended, else to the main agent's last call or tool
// before the next prompt.
func (b *spanBuilder) turns(l sessionLines, tools []toolTime) []turnSpan {
	out := make([]turnSpan, 0, len(l.prompts))
	for i, p := range l.prompts {
		var next time.Time
		if i+1 < len(l.prompts) {
			next = l.prompts[i+1]
		}
		end := turnEnd(l, tools, p, next)
		out = append(out, turnSpan{id: b.add(nameTurn, p, end, "", nil), start: p, end: end})
	}
	return out
}

// inferredTurns adds a span per turn of a session with no prompt line: from its first main-agent
// call's end to the main agent's last call or tool before the next turn's first call.
func (b *spanBuilder) inferredTurns(l sessionLines, tools []toolTime) []turnSpan {
	starts := inferredStarts(l)
	out := make([]turnSpan, 0, len(starts))
	for i, start := range starts {
		var next time.Time
		if i+1 < len(starts) {
			next = starts[i+1]
		}
		in := func(t time.Time) bool { return t.After(start) && (next.IsZero() || t.Before(next)) }
		end := start
		for _, c := range l.calls {
			if c.agent == "" && in(c.end) {
				end = maxTime(end, c.end)
			}
		}
		for _, t := range tools {
			if t.agent == "" && in(t.end) {
				end = maxTime(end, t.end)
			}
		}
		out = append(out, turnSpan{id: b.add(nameTurn, start, end, "", nil), start: start, end: end})
	}
	return out
}

// inferredStarts is when each inferred turn began: the end of the main agent's first call, and of
// its first call after each call that handed back.
func inferredStarts(l sessionLines) []time.Time {
	calls := slices.Clone(l.calls)
	slices.SortStableFunc(calls, func(a, b callTime) int { return a.end.Compare(b.end) })
	var starts []time.Time
	open := false
	for _, c := range calls {
		if c.agent != "" {
			continue
		}
		if !open {
			starts, open = append(starts, c.end), true
		}
		if l.handBack[c.message] {
			open = false
		}
	}
	return starts
}

// turnEnd is when the turn prompted at p ended: the later of its turn.ended and the main
// agent's last call or tool before next (zero for none).
func turnEnd(l sessionLines, tools []toolTime, p, next time.Time) time.Time {
	in := func(t time.Time) bool { return t.After(p) && (next.IsZero() || !t.After(next)) }
	end := p
	if i := slices.IndexFunc(l.stops, in); i >= 0 {
		end = l.stops[i]
	}
	for _, c := range l.calls {
		if c.agent == "" && in(c.end) {
			end = maxTime(end, c.end)
		}
	}
	for _, t := range tools {
		if t.agent == "" && in(t.end) {
			end = maxTime(end, t.end)
		}
	}
	return end
}

// parentOf is the span a call of agent at t sits under: the Agent call that ran a subagent, else
// the turn running then.
func (b *spanBuilder) parentOf(turns []turnSpan, agent string, at time.Time) string {
	if id := b.launcher[agent]; agent != "" && id != "" {
		return id
	}
	return b.turnAt(turns, at)
}

// turnAt is the id of the turn running at t, or "" for none.
func (b *spanBuilder) turnAt(turns []turnSpan, t time.Time) string {
	for _, ts := range turns {
		if !t.Before(ts.start) && !t.After(ts.end) {
			return ts.id
		}
	}
	return ""
}

// instant parses a ledger timestamp, to the second or finer.
func instant(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}
