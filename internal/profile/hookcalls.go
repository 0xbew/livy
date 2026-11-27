package profile

import (
	"slices"
	"time"
)

// Placing model calls from the ledger. usage.api_call gives a call's end to the whole second;
// the tool calls the hooks record are timed to the millisecond. Within one agent the order is
// fixed: a call ends, the tools it asked for run, their results go into the next call. So each
// call is matched to the tools it asked for (its call.detail names them), which places its end
// at the first of them to start, and the next call starts when the last of them ends.

// truncation is how much of a call's end usage.api_call's whole-second timestamp can hide.
const truncation = time.Second

// A call can take at most callFloor plus one callPerOutput per token it wrote. A start that makes
// it longer is not its start: its agent was waiting on something no line records (a call the
// ledger left out as out of scope, a background subagent, a scheduled wake-up), so the call's
// start is unknown. Claude Code's own api_request events put the slowest calls near 30 s plus
// 10 ms a token; this bound is ten times slower.
const (
	callFloor     = 60 * time.Second
	callPerOutput = 100 * time.Millisecond
)

// plausible is whether a call that wrote output tokens could have run from start to end.
func plausible(start, end time.Time, output int64) bool {
	return end.Sub(start) <= callFloor+time.Duration(output)*callPerOutput
}

// placedCall is one model call with its start; placed is false when nothing says when it began.
type placedCall struct {
	callTime
	start  time.Time
	placed bool
}

// placeCalls places every model call of the session, agent by agent.
func placeCalls(l sessionLines, tools []toolTime, launches map[string]time.Time) []placedCall {
	byAgent := map[string][]int{}
	for i, c := range l.calls {
		byAgent[c.agent] = append(byAgent[c.agent], i)
	}
	out := make([]placedCall, len(l.calls))
	for agent, idx := range byAgent {
		p := agentPlacer{l: l, used: map[string]bool{}}
		for _, t := range tools {
			if t.agent == agent {
				p.tools = append(p.tools, t)
			}
		}
		if at, ok := launches[agent]; ok && agent != "" {
			p.prevEnd = at
		}
		for n, i := range idx {
			var next callTime
			if n+1 < len(idx) {
				next = l.calls[idx[n+1]]
			}
			out[i] = p.place(l.calls[i], next)
		}
	}
	return out
}

// agentPlacer walks one agent's calls in order.
type agentPlacer struct {
	l     sessionLines
	tools []toolTime // the agent's tools, by start
	used  map[string]bool
	// prevEnd is when the previous call ended (a subagent's launch before its first), and fed
	// the tools that call asked for.
	prevEnd time.Time
	fed     []toolTime
}

// place times call c; next is its agent's next call (zero for none).
func (p *agentPlacer) place(c, next callTime) placedCall {
	start := p.prevEnd
	for _, t := range p.fed {
		// A tool still running after this call ended was left in the background: it fed nothing.
		if t.end.Before(c.end.Add(truncation)) {
			start = maxTime(start, t.end)
		}
	}
	if c.agent == "" {
		for _, at := range p.l.prompts {
			if !at.After(c.end) {
				start = maxTime(start, at)
			}
		}
	}
	asked := p.asked(c, next)
	end := c.end
	if len(asked) > 0 && asked[0].start.Before(c.end.Add(truncation)) {
		end = maxTime(end, asked[0].start)
	}
	if len(asked) == 0 && !end.After(start) && start.Before(c.end.Add(truncation)) {
		// The call began after its whole-second end: it ended within that second, so halfway.
		end = start.Add(c.end.Add(truncation).Sub(start) / 2)
	}
	p.prevEnd, p.fed = maxTime(end, p.prevEnd), asked
	if len(p.l.prompts) == 0 && p.l.handBack[c.message] {
		// No prompt line says when the next turn began, so its first call has no start.
		p.prevEnd, p.fed = time.Time{}, nil
	}
	c.end = end
	return placedCall{callTime: c, start: start, placed: !start.IsZero() && plausible(start, end, c.output)}
}

// asked is the tools call c asked for, each started after the previous call ended. When its
// call.detail names them, the first unused tool of each name, in start order: Claude Code can
// start a tool while the call that asked for it is still streaming, so a start before the next
// call's end does not say which call asked. Without a call.detail, every unused tool that
// started before the agent's next call's whole-second end.
// (The transcript backfill starts a tool when its tool_use block was written, which can be
// before its call's last record, so the call's own end does not bound it either.)
func (p *agentPlacer) asked(c, nextCall callTime) []toolTime {
	names, known := p.l.launched[c.message]
	names = slices.Clone(names)
	next := nextCall.end
	nextNames := p.l.launched[nextCall.message]
	var out []toolTime
	for _, t := range p.tools {
		if p.used[t.id] || t.start.Before(p.prevEnd) {
			continue
		}
		if !next.IsZero() && !t.start.Before(next.Add(truncation)) {
			break // a later call's: a tool this call asked for that never ran has no line
		}
		if known {
			j := slices.Index(names, t.tool)
			// Within the next call's last second, a tool of a kind the next call asked for is its:
			// one this call asked for that never ran (refused) has no line to match.
			if j < 0 || (!next.IsZero() && !t.start.Before(next) && slices.Contains(nextNames, t.tool)) {
				continue
			}
			names = slices.Delete(names, j, j+1)
		} else if !next.IsZero() && !t.start.Before(next) {
			continue
		}
		p.used[t.id] = true
		out = append(out, t)
	}
	return out
}
