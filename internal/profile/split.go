package profile

// laneTimes are a window's spans by who ran them: the session's own agent's tool calls and model
// calls, and everything a subagent ran (its Agent call and the calls below it).
type laneTimes struct {
	mainTools, mainModel, subagents intervals
}

// add files span s's time in the window, ivs, under the lane that ran it.
func (l *laneTimes) add(f forest, s Span, ivs intervals) {
	switch {
	case s.subagent() || f.inSubagent(s):
		l.subagents = append(l.subagents, ivs...)
	case s.Kind() == KindTool:
		l.mainTools = append(l.mainTools, ivs...)
	default:
		l.mainModel = append(l.mainModel, ivs...)
	}
}

// split cuts busy (the union of every lane) by Split's order: the session's tools, then its model
// calls, then subagents for the rest.
func (l laneTimes) split(busy intervals) Split {
	tools := l.mainTools.union()
	model := l.mainModel.union().minus(tools)
	return Split{Tools: tools.length(), Model: model.length(), Subagents: busy.minus(tools).minus(model).length()}
}

// inSubagent is whether a subagent made s: the span names one (Livy's hook spans do), or an Agent
// call is above it (Claude Code's own spans put a subagent's calls under it).
func (f forest) inSubagent(s Span) bool {
	if s.Attrs["agent_id"] != "" {
		return true
	}
	for p, ok := f.byKey[spanKey{s.Trace, s.Parent}]; ok; p, ok = f.byKey[spanKey{p.Trace, p.Parent}] {
		if p.subagent() {
			return true
		}
	}
	return false
}
