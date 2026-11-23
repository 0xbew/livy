package profile

import (
	"slices"
	"time"
)

// Outcome is how a model or tool call ended, as `livy profile --json` writes it.
type Outcome string

// The outcomes a call can have. OutcomeUnknown is a call whose spans do not say: Codex's, and
// a tool span with no execution and no rejected approval below it.
const (
	OutcomeUnknown  Outcome = ""
	OutcomeOK       Outcome = "ok"
	OutcomeFailed   Outcome = "failed"
	OutcomeRejected Outcome = "rejected" // the human (or a permission rule) refused the tool call
)

// toolExecution is Claude Code's span for a tool call actually running, under its tool span.
const toolExecution = "claude_code.tool.execution"

// outcome is how call s ended: a model call by its own success attribute, a tool call by its
// execution's.
func (f forest) outcome(s Span) Outcome {
	switch s.Kind() {
	case KindModel:
		return successOutcome(s)
	case KindTool:
		return f.toolOutcome(s)
	case KindTurn, KindWait, KindOther:
	}
	return OutcomeUnknown
}

// toolOutcome is how tool call s ended: by its execution's success, or rejected when an
// approval below it was refused and it never ran.
func (f forest) toolOutcome(s Span) Outcome {
	rejected := false
	for _, c := range f.children[spanKey{s.Trace, s.ID}] {
		switch {
		case c.Name == toolExecution:
			return successOutcome(c)
		case c.Kind() == KindWait && c.Attrs["decision"] == "reject":
			rejected = true
		}
	}
	if rejected {
		return OutcomeRejected
	}
	return OutcomeUnknown
}

// successOutcome reads Claude Code's boolean success attribute.
func successOutcome(s Span) Outcome {
	switch s.Attrs["success"] {
	case "true":
		return OutcomeOK
	case "false":
		return OutcomeFailed
	}
	return OutcomeUnknown
}

// WaitKind is why the human was being waited on.
type WaitKind string

// The kinds of wait: between turns (the agent had handed back), or for an approval inside one.
const (
	WaitBetweenTurns WaitKind = "between_turns"
	WaitApproval     WaitKind = "approval"
)

// Wait is one stretch of a window spent waiting on the human, with no agent work running.
type Wait struct {
	Kind       WaitKind
	Start, End time.Time
}

// waitsOf splits the window's time waiting on the human, oldest first: the time between turns
// and the approval waits, each without agent time (busy, a union), and the approval waits also
// without the time between turns, so the stretches never overlap and their lengths sum to
// Profile.Wait.
func waitsOf(between, approvals, busy intervals) []Wait {
	idle := between.minus(busy)
	asked := approvals.minus(append(slices.Clone(busy), between...).union())
	out := make([]Wait, 0, len(idle)+len(asked))
	for _, i := range idle {
		out = append(out, Wait{Kind: WaitBetweenTurns, Start: i.from, End: i.to})
	}
	for _, i := range asked {
		out = append(out, Wait{Kind: WaitApproval, Start: i.from, End: i.to})
	}
	slices.SortStableFunc(out, func(a, b Wait) int { return a.Start.Compare(b.Start) })
	return out
}
