package profile

import (
	"maps"
	"time"

	"github.com/0xbew/livy/internal/why"
)

// JSONSchema names the shape `livy profile --json` writes. Fields are only ever added; a
// change that removes or retypes one is a new schema value. Durations are whole milliseconds.
const JSONSchema = "livy.profile/1"

// JSONReport is `livy profile --json`: every commit in the range, oldest first, the totals
// and what was read.
type JSONReport struct {
	Schema  string       `json:"schema"`
	Commits []JSONCommit `json:"commits"`
	Totals  JSONTotals   `json:"totals"`
	Read    JSONRead     `json:"read"`
}

// JSONCommit is where one commit's time went.
type JSONCommit struct {
	SHA       string        `json:"sha"`
	Committed time.Time     `json:"committed"`
	Rework    JSONRework    `json:"rework"`
	Sessions  []JSONSession `json:"sessions"`
	// Squash is the pull request the commit merged and each of its commits' profile; null
	// when no pull request was looked up.
	Squash *JSONSquash `json:"squash"`
}

// JSONSquash is the pull request a commit merged.
type JSONSquash struct {
	PR      int          `json:"pr"` // 0 when none was found
	Merge   bool         `json:"merge_commit"`
	Problem string       `json:"problem"`
	Commits []JSONCommit `json:"commits"`
}

// JSONRework is the rework signals profile can see for a commit.
type JSONRework struct {
	Fixup    bool           `json:"fixup"`
	Rewrites map[string]int `json:"rewrites"`
	// FilesAgain is how many of the commit's files earlier commits of its sessions changed, and
	// EarlierCommits how many such commits there were; Problem says why they could not be read.
	FilesAgain     int    `json:"files_changed_again"`
	EarlierCommits int    `json:"earlier_commits"`
	Problem        string `json:"problem"`
}

// JSONSession is one session's time before a commit. Start is null when the window has no
// prompt and no span: nothing to time. The time split, the turns and the waits are set only
// when Timed.
type JSONSession struct {
	// WindowBasisKind and WindowBasisSHA are how the window's start was found, as livy.why/1's
	// window_basis_kind and window_basis_sha say it.
	WindowBasisKind string `json:"window_basis_kind"`
	WindowBasisSHA  string `json:"window_basis_sha"`

	ID          string     `json:"session_id"`
	Harness     string     `json:"harness"`
	WindowFrom  *time.Time `json:"window_from"`
	WindowTo    time.Time  `json:"window_to"`
	Start       *time.Time `json:"start"`
	WallMS      int64      `json:"wall_ms"`
	Timed       bool       `json:"timed"`
	AgentMS     int64      `json:"agent_ms"`
	ModelMS     int64      `json:"model_ms"`
	WaitMS      int64      `json:"waiting_on_human_ms"`
	OtherMS     int64      `json:"other_ms"`
	Turns       int        `json:"turns"`
	SpansPlaced int        `json:"spans_placed"`
	SpansTotal  int        `json:"spans_total"`
	Longest     []JSONSpan `json:"longest"`
	// TurnTimes are the window's turns, oldest first; Waits the stretches waiting on the human,
	// oldest first, which never overlap and whose times (not their rounded-down DurationMS) sum to
	// WaitMS.
	TurnTimes []JSONTurn `json:"turn_times"`
	Waits     []JSONWait `json:"waits"`
	// Prompts is how many prompts a person typed in the window, as livy.why/1's prompt_ids;
	// AgentBriefs how many agent-written briefs, as its agent_brief_ids.
	Prompts     int `json:"prompts"`
	AgentBriefs int `json:"agent_briefs"`
	// TurnsInferred is true for a window timed from hooks with no prompt line: each turn runs from
	// its first model call's end to its last call or tool, so the wait before a turn holds its
	// first call too.
	TurnsInferred bool `json:"turns_inferred"`
	// TimingSource is where a timed window's times came from: "otel" (spans) or "hooks" (Livy's
	// own ledger lines, when no span reached Livy); "" when the window is not timed.
	TimingSource string `json:"timing_source"`
	// Spans is a timed window's trace: every span in its turns' trees, in tree order; Agents the
	// lanes they ran in, main first; Markers its skill loads and compactions. Empty when untimed.
	Spans   []JSONTraceSpan `json:"spans"`
	Agents  []JSONAgent     `json:"agents"`
	Markers []JSONMarker    `json:"markers"`
}

// JSONTurn is one turn, clipped to the window.
type JSONTurn struct {
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	DurationMS int64     `json:"duration_ms"`
}

// JSONWait is one stretch of waiting on the human with no agent work running. Kind is
// "between_turns" (the agent had handed back) or "approval" (a tool call waited for one).
type JSONWait struct {
	Kind       string    `json:"kind"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	DurationMS int64     `json:"duration_ms"`
}

// JSONSpan is one model call, tool or subagent, clipped to the window. Kind is "model",
// "tool" or "subagent"; Name is the model or the tool; Label is both as the text prints them.
// Agent is "main" for the session's own calls, else the id of the subagent that made it, and
// AgentType that subagent's type when the ledger names it. A subagent span's StartsAgent is
// the id of the subagent it ran, when one of its calls names it. Outcome is "ok", "failed",
// "rejected" (an approval was refused, so the tool never ran) or "" when the spans do not say.
type JSONSpan struct {
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	Label       string    `json:"label"`
	Start       time.Time `json:"start"`
	DurationMS  int64     `json:"duration_ms"`
	Agent       string    `json:"agent"`
	AgentType   string    `json:"agent_type"`
	StartsAgent string    `json:"starts_agent"`
	Outcome     string    `json:"outcome"`
}

// JSONTotals are the sums over every timed window in the report.
type JSONTotals struct {
	Commits int `json:"commits"`
	Windows int `json:"windows"`
	Timed   int `json:"timed"`
	// TimedFromHooks is how many of the timed windows were timed from the ledger, not spans.
	TimedFromHooks int   `json:"timed_from_hooks"`
	WallMS         int64 `json:"wall_ms"`
	AgentMS        int64 `json:"agent_ms"`
	WaitMS         int64 `json:"waiting_on_human_ms"`
	Fixups         int   `json:"fixup_commits"`
	Rewritten      int   `json:"rewritten_commits"`
}

// JSONRead is what the report read: why's lines, the spans, and what could not be read.
type JSONRead struct {
	Summary   string   `json:"summary"`
	Spans     int      `json:"spans"`
	SpanFiles int      `json:"span_files"`
	Bad       int      `json:"unreadable_files_or_lines"`
	Unread    []string `json:"unread"`
}

// jsonBuilder makes a JSONReport, keeping the totals as report does.
type jsonBuilder struct {
	spans  []Span
	events []why.Event
	top    int
	totals JSONTotals
	// agentTypes is each subagent's type by its id, from the ledger.
	agentTypes map[string]string
	files      commitFiles
	// calls is what a model span joins to: its usage.api_call and cache verdict.
	calls calls
	// markers are each session's skill loads and compactions, oldest first.
	markers map[string][]JSONMarker
	// targets are the tool calls' kept targets (package tooltarget), by tool use id.
	targets map[string]string
}

func (b *jsonBuilder) commit(e why.Explained) JSONCommit {
	c := e.Commit
	b.totals.Commits++
	rw := reworkOf(c, e.Report.Sessions, b.events, &b.files)
	b.totals.Fixups += b2i(rw.fixup)
	b.totals.Rewritten += b2i(len(rw.rewrites) > 0)
	out := JSONCommit{SHA: c.SHA, Committed: c.Committed.UTC(), Sessions: make([]JSONSession, 0, len(e.Report.Sessions)),
		Rework: JSONRework{Fixup: rw.fixup, Rewrites: maps.Clone(rw.rewrites), FilesAgain: rw.again,
			EarlierCommits: rw.earlier, Problem: rw.problem}}
	if out.Rework.Rewrites == nil {
		out.Rework.Rewrites = map[string]int{}
	}
	for _, s := range e.Report.Sessions {
		out.Sessions = append(out.Sessions, b.session(s))
	}
	if sq := e.Squash; sq != nil {
		js := &JSONSquash{PR: sq.PR, Merge: sq.Merge, Problem: sq.Problem, Commits: make([]JSONCommit, 0, len(sq.Originals))}
		if len(sq.Originals) == 0 && js.Problem == "" {
			js.Problem = "no pull request found that merged it"
		}
		for _, o := range sq.Originals {
			js.Commits = append(js.Commits, b.commit(o))
		}
		out.Squash = js
	}
	return out
}

func (b *jsonBuilder) session(s why.Session) JSONSession {
	p, src := profileOf(b.spans, b.events, s)
	b.totals.Windows++
	kind, sha := why.BasisKind(s.Basis)
	out := JSONSession{WindowBasisKind: kind, WindowBasisSHA: sha, ID: s.ID, Harness: s.Harness, WindowFrom: why.TimeOrNull(s.From), WindowTo: s.To.UTC(),
		Start: why.TimeOrNull(p.Start), Timed: p.Timed, Turns: len(p.Turns), SpansPlaced: p.Placed, SpansTotal: p.Total,
		Longest: []JSONSpan{}, TurnTimes: []JSONTurn{}, Waits: []JSONWait{}, Spans: []JSONTraceSpan{}, Agents: []JSONAgent{},
		Markers: []JSONMarker{}}
	out.Prompts, out.AgentBriefs = len(s.Prompts), len(s.AgentBriefs)
	if p.Start.IsZero() {
		return out
	}
	out.WallMS = p.Wall.Milliseconds()
	if !p.Timed {
		return out
	}
	b.totals.Timed++
	b.totals.TimedFromHooks += b2i(src == SourceHooks)
	out.TimingSource = string(src)
	out.TurnsInferred = p.TurnsInferred
	b.totals.WallMS += p.Wall.Milliseconds()
	b.totals.AgentMS += p.Busy.Milliseconds()
	b.totals.WaitMS += p.Wait.Milliseconds()
	out.AgentMS, out.ModelMS, out.WaitMS, out.OtherMS = p.Busy.Milliseconds(), p.Model.Milliseconds(), p.Wait.Milliseconds(), p.Other().Milliseconds()
	out.Spans, out.Agents, out.Markers = b.traceOf(p, s.ID)
	for _, n := range longestOf(p, b.top) {
		out.Longest = append(out.Longest, b.span(n))
	}
	for _, t := range p.Turns {
		out.TurnTimes = append(out.TurnTimes, JSONTurn{Start: t.Start.UTC(), End: t.End.UTC(), DurationMS: t.Duration().Milliseconds()})
	}
	for _, w := range p.Waits {
		out.Waits = append(out.Waits, JSONWait{Kind: string(w.Kind), Start: w.Start.UTC(), End: w.End.UTC(),
			DurationMS: w.End.Sub(w.Start).Milliseconds()})
	}
	return out
}

// mainAgent is the agent a session's own calls belong to.
const mainAgent = "main"

// span is one of the longest spans, with the agent that made it and how it ended.
func (b *jsonBuilder) span(n *Node) JSONSpan {
	kind, name := spanKindName(n.Span)
	out := JSONSpan{Kind: kind, Name: name, Label: n.Span.Label(), Start: n.Start.UTC(),
		DurationMS: n.Duration().Milliseconds(), Agent: mainAgent, Outcome: string(n.Outcome)}
	if id := n.Span.Attrs["agent_id"]; id != "" {
		out.Agent, out.AgentType = id, b.agentTypes[id]
	}
	if kind == "subagent" {
		out.StartsAgent = startedAgent(n)
	}
	return out
}

// startedAgent is the id of the subagent a subagent span ran: the first of its calls to name one.
func startedAgent(n *Node) string {
	for _, c := range n.Children {
		if id := c.Span.Attrs["agent_id"]; id != "" {
			return id
		}
	}
	return ""
}

// agentTypesOf is each subagent's type by its id, from the ledger lines that name both
// (extraction's call.detail lines).
func agentTypesOf(events []why.Event) map[string]string {
	out := map[string]string{}
	for _, e := range events {
		if e.AgentID != "" && e.SubagentType != "" {
			out[e.AgentID] = e.SubagentType
		}
	}
	return out
}

// spanKindName is a longest span's kind and the model or tool it names, as fixed words.
func spanKindName(s Span) (kind, name string) {
	switch s.Kind() {
	case KindModel:
		return "model", s.Attrs["model"]
	case KindTool:
		if s.subagent() {
			return "subagent", s.Attrs["tool_name"]
		}
		return "tool", s.Attrs["tool_name"]
	case KindTurn, KindWait, KindOther:
	}
	return "other", s.Name
}
