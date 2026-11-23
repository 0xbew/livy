package profile

import (
	"maps"
	"slices"
	"time"
)

// Window is one session's share of one commit: the session, the previous commit's time (zero
// for none), the commit's time, and when its prompts and its agent-written brief were sent.
type Window struct {
	Session string
	From    time.Time
	End     time.Time
	Prompts []time.Time
	Briefs  []time.Time
}

// Profile is where one session's time went before one commit.
type Profile struct {
	Start, End time.Time
	// Wall is End - Start; Busy is agent time (model calls and tools), Model the part of it in
	// model calls, Wait the time waiting on the human: between turns or for an approval.
	Wall, Busy, Model, Wait time.Duration
	Turns                   []*Node
	// Waits are the stretches that make up Wait, oldest first.
	Waits []Wait
	// Placed of Total: the session's spans in the window, and how many sit under a turn.
	Placed, Total int
	// Busy and Wait are known only when a turn span is in the window.
	Timed bool
}

// Other is wall time that is neither agent time nor waiting on the human: the harness's own
// work inside a turn, hooks, and time no span covers.
func (p Profile) Other() time.Duration { return p.Wall - p.Busy - p.Wait }

// Node is one span in a profile's tree, its times clipped to the window, and how a model or
// tool call ended.
type Node struct {
	Span       Span
	Start, End time.Time
	Outcome    Outcome
	Children   []*Node
}

// Duration is the node's time inside the window.
func (n *Node) Duration() time.Duration { return n.End.Sub(n.Start) }

// spanKey is a span's identity: span ids are unique within a trace.
type spanKey struct{ trace, id string }

// forest is the spans indexed by identity and by parent.
type forest struct {
	byKey    map[spanKey]Span
	children map[spanKey][]Span
}

func newForest(spans []Span) forest {
	f := forest{byKey: map[spanKey]Span{}, children: map[spanKey][]Span{}}
	for _, s := range spans {
		k := spanKey{s.Trace, s.ID}
		if _, dup := f.byKey[k]; dup {
			continue // an export read twice (a kept segment and a --data copy)
		}
		f.byKey[k] = s
		if s.Parent != "" {
			p := spanKey{s.Trace, s.Parent}
			f.children[p] = append(f.children[p], s)
		}
	}
	for k := range f.children {
		slices.SortStableFunc(f.children[k], func(a, b Span) int { return a.Start.Compare(b.Start) })
	}
	return f
}

// descend calls visit on s and every span below it.
func (f forest) descend(s Span, visit func(Span)) {
	visit(s)
	for _, c := range f.children[spanKey{s.Trace, s.ID}] {
		f.descend(c, visit)
	}
}

// session is the session's turns, and every span that is under one of them or names the
// session itself (Claude Code's spans all do; Codex names it on the turn).
func (f forest) session(id string) (turns []Span, members map[spanKey]Span) {
	members = map[spanKey]Span{}
	for k, s := range f.byKey {
		if !s.names(id) {
			continue
		}
		members[k] = s
		if s.Kind() == KindTurn {
			turns = append(turns, s)
			f.descend(s, func(d Span) { members[spanKey{d.Trace, d.ID}] = d })
		}
	}
	slices.SortStableFunc(turns, func(a, b Span) int { return a.Start.Compare(b.Start) })
	return turns, members
}

// overlaps says whether s runs at some point after from and before end; a zero from is open.
func overlaps(s Span, from, end time.Time) bool {
	return s.Start.Before(end) && (from.IsZero() || s.End.After(from))
}

// Build is the profile of session w.Session between w.From and w.End from spans. Its start is
// the first prompt sent in the window, or the previous commit when a turn was already running
// then; zero (and no times) when neither a prompt nor a span is in the window.
func Build(spans []Span, w Window) Profile {
	f := newForest(spans)
	turns, members := f.session(w.Session)
	p := Profile{End: w.End, Start: start(w, turns)}
	if p.Start.IsZero() {
		return p
	}
	p.Wall = p.End.Sub(p.Start)
	placed := map[spanKey]bool{}
	var turnTimes intervals
	for _, t := range turns {
		if !overlaps(t, p.Start, p.End) {
			continue
		}
		p.Turns = append(p.Turns, f.node(t, p.Start, p.End, placed))
		turnTimes = append(turnTimes, clip(t, p.Start, p.End))
	}
	var model, tool, wait intervals
	for _, s := range members {
		if !overlaps(s, p.Start, p.End) {
			continue
		}
		p.Total++
		switch s.Kind() {
		case KindModel:
			model = append(model, clip(s, p.Start, p.End))
		case KindTool:
			// An approval wait blocks its own tool (and a subagent above it), not other work.
			tool = append(tool, intervals{clip(s, p.Start, p.End)}.minus(f.waitsUnder(s, p.Start, p.End))...)
		case KindWait:
			wait = append(wait, clip(s, p.Start, p.End))
		}
	}
	p.Placed = len(placed)
	if len(p.Turns) == 0 {
		return p // no turn span: OTEL was off or the receiver was down, so the split is unknown
	}
	p.Timed = true
	busy := append(slices.Clone(model), tool...).union()
	between := intervals{{p.Start, p.End}}.minus(turnTimes.union())
	p.Busy = busy.length()
	p.Model = model.union().length()
	p.Wait = append(wait, between...).union().minus(busy).length()
	p.Waits = waitsOf(between, wait, busy)
	return p
}

// start is when the window's work began: its first prompt or brief, or the start of the first of the
// session's turns running in it (clipped to the previous commit), whichever is earlier. Only a
// turn counts: a session-level span (Codex's session_loop) also runs over idle time.
func start(w Window, turns []Span) time.Time {
	var first time.Time
	earlier := func(t time.Time) {
		if first.IsZero() || t.Before(first) {
			first = t
		}
	}
	for _, at := range slices.Concat(w.Prompts, w.Briefs) {
		if !at.Before(w.From) && !at.After(w.End) {
			earlier(at)
		}
	}
	for _, t := range turns {
		if overlaps(t, w.From, w.End) {
			earlier(maxTime(t.Start, w.From))
		}
	}
	return first
}

// waitsUnder is the approval waits below s, clipped to [from, end], as a union.
func (f forest) waitsUnder(s Span, from, end time.Time) intervals {
	var out intervals
	f.descend(s, func(d Span) {
		if d.Kind() == KindWait {
			out = append(out, clip(d, from, end))
		}
	})
	return out.union()
}

// node is s's tree inside [from, end]: each span below it that runs in the window, under its
// nearest printed ancestor. Every span reached is marked placed.
func (f forest) node(s Span, from, end time.Time, placed map[spanKey]bool) *Node {
	placed[spanKey{s.Trace, s.ID}] = true
	iv := clip(s, from, end)
	if s.Kind() == KindModel && s.Attrs["model"] == "" {
		s.Attrs = maps.Clone(s.Attrs)
		s.Attrs["model"] = f.ancestorModel(s)
	}
	n := &Node{Span: s, Start: iv.from, End: iv.to, Outcome: f.outcome(s)}
	f.attach(n, s, from, end, placed)
	return n
}

// attach adds the spans below s to n: a printed kind as a child node, any other kind's own
// children in its place.
func (f forest) attach(n *Node, s Span, from, end time.Time, placed map[spanKey]bool) {
	for _, c := range f.children[spanKey{s.Trace, s.ID}] {
		if !overlaps(c, from, end) {
			continue
		}
		if c.Kind() == KindOther {
			placed[spanKey{c.Trace, c.ID}] = true
			f.attach(n, c, from, end, placed)
			continue
		}
		n.Children = append(n.Children, f.node(c, from, end, placed))
	}
}

// ancestorModel is the model the nearest span above s names: Codex names it on the sampling
// request, not on the sampling span under it.
func (f forest) ancestorModel(s Span) string {
	for p, ok := f.byKey[spanKey{s.Trace, s.Parent}]; ok; p, ok = f.byKey[spanKey{p.Trace, p.Parent}] {
		if m := p.Attrs["model"]; m != "" {
			return m
		}
	}
	return ""
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
