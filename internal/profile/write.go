package profile

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/why"
)

// childrenShown is how many spans a node lists under it unless --all.
const childrenShown = 25

// shownWait is the shortest approval wait a tool's line mentions: shorter rounds to 0.0s.
const shownWait = 50 * time.Millisecond

// report writes profiles and keeps the totals for the closing lines.
type report struct {
	w      io.Writer
	spans  []Span
	events []why.Event
	top    int
	all    bool
	files  commitFiles

	windows, timed, placed, total int
	wall, busy, wait              time.Duration
	commits, fixups, rewritten    int
}

// commit writes one commit's profile, and under a squash merge each of its pull request's
// commits; indent prefixes every line.
func (r *report) commit(e why.Explained, indent string) {
	c := e.Commit
	r.commits++
	fmt.Fprintf(r.w, "%scommit %s  committed %s\n", indent, why.Short(c.SHA), c.Committed.Format(time.RFC3339))
	rw := reworkOf(c, e.Report.Sessions, r.events, &r.files)
	r.fixups += b2i(rw.fixup)
	r.rewritten += b2i(len(rw.rewrites) > 0)
	fmt.Fprintf(r.w, "%s  rework: %s\n", indent, rw.String())
	if len(e.Report.Sessions) == 0 && e.Squash == nil {
		fmt.Fprintf(r.w, "%s  no agent session: no Agent-Session trailer and no commit.recorded line for it here\n", indent)
	}
	for _, s := range e.Report.Sessions {
		r.session(s, indent+"  ")
	}
	if sq := e.Squash; sq != nil {
		if len(sq.Originals) == 0 {
			fmt.Fprintf(r.w, "%s  %s\n", indent, cmp.Or(sq.Problem, "no pull request found that merged it"))
			return
		}
		if sq.Problem != "" {
			fmt.Fprintf(r.w, "%s  %s\n", indent, sq.Problem)
		}
		fmt.Fprintf(r.w, "%s  %s; the time behind each:\n", indent, sq.Describe())
		for _, o := range sq.Originals {
			r.commit(o, indent+"    ")
		}
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// session writes one session's profile for the commit.
func (r *report) session(s why.Session, indent string) {
	w := windowOf(s)
	p := Build(r.spans, w)
	r.windows++
	r.placed += p.Placed
	r.total += p.Total
	fmt.Fprintf(r.w, "%ssession %s (%s)\n", indent, s.ID, why.OrUnknown(s.Harness))
	if p.Start.IsZero() {
		fmt.Fprintf(r.w, "%s  no prompt and no span in this window: nothing to time\n", indent)
		return
	}
	fmt.Fprintf(r.w, "%s  wall %s, from %s to the commit\n", indent, dur(p.Wall), startedBy(p, w, s.Basis))
	if !p.Timed {
		fmt.Fprintf(r.w, "%s  no turn span in this window (OTEL off, or Livy's receiver was down): agent and wait time unknown\n", indent)
		return
	}
	r.timed++
	r.wall += p.Wall
	r.busy += p.Busy
	r.wait += p.Wait
	fmt.Fprintf(r.w, "%s  agent %s (model %s, tools %s), waiting on the human %s, other %s\n", indent,
		dur(p.Busy), dur(p.Model), dur(p.Busy-p.Model), dur(p.Wait), dur(p.Other()))
	fmt.Fprintf(r.w, "%s  spans: %d of %d in the tree\n", indent, p.Placed, p.Total)
	for _, t := range p.Turns {
		fmt.Fprintf(r.w, "%s  turn at %s  %s\n", indent, t.Start.Format(time.RFC3339), dur(t.Duration()))
		r.children(t, indent+"    ")
	}
	r.longest(p, indent+"  ")
}

// windowOf is the window profile times for one of why's sessions.
func windowOf(s why.Session) Window {
	w := Window{Session: s.ID, From: s.From, End: s.To}
	for _, at := range s.Sent {
		if !at.IsZero() {
			w.Prompts = append(w.Prompts, at)
		}
	}
	for _, at := range s.BriefSent {
		if !at.IsZero() {
			w.Briefs = append(w.Briefs, at)
		}
	}
	return w
}

// startedBy says what the window's start was; basis is how why found the window's start.
func startedBy(p Profile, w Window, basis string) string {
	for _, at := range w.Prompts {
		if at.Equal(p.Start) {
			return "the first prompt at " + p.Start.Format(time.RFC3339)
		}
	}
	if slices.ContainsFunc(w.Briefs, p.Start.Equal) {
		return "the agent-written brief at " + p.Start.Format(time.RFC3339)
	}
	if p.Start.Equal(w.From) {
		kind, _ := why.BasisKind(basis)
		return "the window's start at " + p.Start.Format(time.RFC3339) + " (basis " + kind + ": " + basis + "; a turn was running)"
	}
	return "the first turn at " + p.Start.Format(time.RFC3339)
}

// children writes n's printed children, each with its own below it.
func (r *report) children(n *Node, indent string) {
	shown := 0
	for _, c := range n.Children {
		if c.Span.Kind() == KindWait {
			continue // said on its tool's line
		}
		if !r.all && shown == childrenShown {
			fmt.Fprintf(r.w, "%s... %d more spans (--all prints them)\n", indent, countPrinted(n.Children)-shown)
			return
		}
		shown++
		line := fmt.Sprintf("%s%s  %s", indent, c.Span.Label(), dur(c.Duration()))
		if waited := approvalWait(c); waited >= shownWait {
			line += ", " + dur(waited) + " of it waiting on approval"
		}
		fmt.Fprintln(r.w, line)
		r.children(c, indent+"  ")
	}
}

func countPrinted(ns []*Node) int {
	k := 0
	for _, n := range ns {
		if n.Span.Kind() != KindWait {
			k++
		}
	}
	return k
}

// approvalWait is the time a tool call waited on the human's approval.
func approvalWait(n *Node) time.Duration {
	var d time.Duration
	for _, c := range n.Children {
		if c.Span.Kind() == KindWait {
			d += c.Duration()
		}
	}
	return d
}

// longest writes the window's longest model calls, tools and subagents.
func (r *report) longest(p Profile, indent string) {
	if r.top == 0 {
		return
	}
	fmt.Fprintf(r.w, "%slongest:\n", indent)
	for _, n := range longestOf(p, r.top) {
		fmt.Fprintf(r.w, "%s  %8s  %s at %s\n", indent, dur(n.Duration()), n.Span.Label(), n.Start.Format(time.RFC3339))
	}
}

// longestOf is the window's top longest model calls, tools and subagents, longest first.
func longestOf(p Profile, top int) []*Node {
	var all []*Node
	var collect func(ns []*Node)
	collect = func(ns []*Node) {
		for _, n := range ns {
			if k := n.Span.Kind(); k == KindModel || k == KindTool {
				all = append(all, n)
			}
			collect(n.Children)
		}
	}
	collect(p.Turns)
	slices.SortStableFunc(all, func(a, b *Node) int { return cmp.Compare(b.Duration(), a.Duration()) })
	return all[:min(top, len(all))]
}

// summary writes the totals over every commit and what was read.
func (r *report) summary(read spanSources) {
	fmt.Fprintf(r.w, "total: %d commits, %d session windows (%d timed): wall %s, agent %s, waiting on the human %s; "+
		"%d fix-up commits, %d rewritten\n", r.commits, r.windows, r.timed, dur(r.wall), dur(r.busy), dur(r.wait),
		r.fixups, r.rewritten)
	fmt.Fprintf(r.w, "spans: %d of %d in the windows sit in a turn's tree; read %d spans from %d files", r.placed,
		r.total, read.spans, read.files)
	if read.bad > 0 {
		fmt.Fprintf(r.w, "; %d unreadable files or lines", read.bad)
	}
	fmt.Fprintln(r.w)
	for _, d := range read.missing {
		fmt.Fprintf(r.w, "%s could not be read\n", d)
	}
}

// dur is a duration as a profile prints it: tenths of a second under a minute.
func dur(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	text := d.Round(time.Second).String()
	if strings.HasSuffix(text, "m0s") {
		return strings.TrimSuffix(text, "0s") // 2m0s reads 2m; 1m10s stays whole
	}
	return text
}
