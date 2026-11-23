package why

import (
	"reflect"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// Guards: an on-call pointed at the wrong agent work. A commit's prompts are the ones its
// session submitted after the previous commit in that session and branch and up to this one:
// not the whole session (one real session spanned 190 PRs), not another session's, not work
// on another branch. A rebased commit is found under the SHA its commit.recorded named, and
// tokens are counted once per API call even when extraction wrote the call twice.
func TestExplain(t *testing.T) {
	const s, other = "s-1", "s-2"
	events := []Event{
		{Event: "prompt.submitted", TS: "2026-11-12T10:00:00+00:00", SessionID: s, PromptID: "p-1", Branch: "f"},
		{Event: "commit.recorded", TS: "2026-11-12T10:10:00+00:00", SessionID: s, PromptID: "p-1", Branch: "f", CommitSHA: "aaa", Harness: "claude-code", TicketKey: "PROJ-1"},
		{Event: "prompt.submitted", TS: "2026-11-12T10:20:00+00:00", SessionID: s, PromptID: "p-2", Branch: "f"},
		{Event: "prompt.submitted", TS: "2026-11-12T10:21:00+00:00", SessionID: other, PromptID: "q-1", Branch: "f"},
		{Event: "commit.recorded", TS: "2026-11-12T10:25:00+00:00", SessionID: s, PromptID: "p-2", Branch: "g", CommitSHA: "ggg", Harness: "claude-code"},
		{Event: "tool.call", TS: "2026-11-12T10:22:00+00:00", SessionID: s, Tool: "Bash"},
		{Event: "tool.call", TS: "2026-11-12T10:23:00+00:00", SessionID: s, Tool: "Bash"},
		{Event: "tool.call", TS: "2026-11-12T10:23:00+00:00", SessionID: other, Tool: "Edit"},
		{Event: "usage.api_call", TS: "2026-11-12T10:24:00+00:00", SessionID: s, Model: "claude-opus-5-5", MessageID: "m1", RequestID: "r1", InputTokens: 10, OutputTokens: 5, CacheReadTokens: 100},
		{Event: "usage.api_call", TS: "2026-11-12T10:24:00+00:00", SessionID: s, Model: "claude-opus-5-5", MessageID: "m1", RequestID: "r1", InputTokens: 10, OutputTokens: 5, CacheReadTokens: 100},
		{Event: "usage.api_call", TS: "2026-11-12T10:05:00+00:00", SessionID: s, Model: "claude-opus-5-5", MessageID: "m0", RequestID: "r0", InputTokens: 999},
		{Event: "prompt.submitted", TS: "2026-11-12T10:30:00+00:00", SessionID: s, PromptID: "p-3", Branch: "f"},
		{Event: "commit.recorded", TS: "2026-11-12T10:40:00+00:00", SessionID: s, PromptID: "p-3", Branch: "f", CommitSHA: "bbb", Harness: "claude-code", TicketKey: "PROJ-1"},
		{Event: "commit.rewritten", TS: "2026-11-12T11:00:00+00:00", SessionID: s, Branch: "f", CommitSHA: "ccc", OldCommitSHA: "bbb", RewriteKind: "rebase"},
	}
	got := Explain(Commit{SHA: "ccc", Committed: at("2026-11-12T11:00:00Z"), Sessions: []string{s}}, events)
	want := Report{SHA: "ccc", Sessions: []Session{{
		ID: s, Harness: "claude-code", From: at("2026-11-12T10:10:00Z"), To: at("2026-11-12T10:40:00Z"),
		Basis:   "commit.recorded aaa",
		Prompts: []string{"p-2", "p-3"},
		Sent:    []time.Time{at("2026-11-12T10:20:00Z"), at("2026-11-12T10:30:00Z")},
		Tools:   map[string]int{"Bash": 2},
		Models:  map[string]Usage{"claude-opus-5-5": {Calls: 1, Input: 10, Output: 5, CacheRead: 100}},
		Ticket:  "PROJ-1",
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Explain(ccc) =\n%+v\nwant\n%+v", got, want)
	}
}

// Guards: a commit with no commit.recorded line whose window runs from the session's start,
// over-attributing prompts and tokens. Its previous commit in the session is the nearest
// first-parent ancestor with that Agent-Session trailer, whatever its committer time (a rebase
// reorders those, and git keeps whole seconds), and an ancestor without an Agent-Model trailer
// counts like any other: git log leaves that field empty.
func TestWindowFromTrailers(t *testing.T) {
	sep := "\x00"
	out := "ccc" + sep + "2026-11-12T10:00:00Z" + sep + "s-1" + sep + "claude-code@2" + sep + "" + "\x1e\n" +
		"bbb" + sep + "2026-11-12T10:00:00Z" + sep + "s-2" + sep + "" + sep + "" + "\x1e\n" +
		"aaa" + sep + "2026-11-12T10:30:00Z" + sep + "s-1" + sep + "" + sep + "" + "\x1e\n"
	ancestors := parseLog(out)
	if len(ancestors) != 3 {
		t.Fatalf("parseLog read %d commits, want 3: %+v", len(ancestors), ancestors)
	}
	c := Commit{SHA: "ddd", Committed: at("2026-11-12T10:00:00Z"), Sessions: []string{"s-1"}, Ancestors: ancestors}
	got := Explain(c, nil)
	if len(got.Sessions) != 1 || got.Sessions[0].Basis != basisTrailer+"ccc" || !got.Sessions[0].From.Equal(at("2026-11-12T10:00:00Z")) {
		t.Errorf("Explain(ddd) = %+v, want the window to start at ccc's trailer", got)
	}
}

// Guards: tools and tokens credited to the wrong commit at a window's edges. Two commits
// recorded in one second are told apart by ledger order, not dropped as an empty window; and a
// commit with no commit.recorded line of its own takes its session's previous commit from its
// own history, never from a commit recorded on another branch.
func TestWindowEdges(t *testing.T) {
	t.Run("two commits in one second", func(t *testing.T) {
		const ts = "2026-11-12T10:00:00+00:00"
		events := []Event{
			{Event: "commit.recorded", TS: ts, SessionID: "s", Branch: "f", CommitSHA: "aaa"},
			{Event: "tool.call", TS: ts, SessionID: "s", Tool: "Bash"},
			{Event: "commit.recorded", TS: ts, SessionID: "s", Branch: "f", CommitSHA: "bbb"},
			{Event: "tool.call", TS: ts, SessionID: "s", Tool: "Edit"},
		}
		got := Explain(Commit{SHA: "bbb", Committed: at("2026-11-12T10:00:00Z")}, events)
		if len(got.Sessions) != 1 || !reflect.DeepEqual(got.Sessions[0].Tools, map[string]int{"Bash": 1}) {
			t.Errorf("Explain(bbb) = %+v, want tools Bash 1 (the call between aaa and bbb)", got)
		}
	})
	t.Run("no commit.recorded of its own", func(t *testing.T) {
		events := []Event{{Event: "commit.recorded", TS: "2026-11-12T10:30:00+00:00", SessionID: "s", Branch: "other", CommitSHA: "xxx"}}
		c := Commit{SHA: "ddd", Committed: at("2026-11-12T11:00:00Z"), Sessions: []string{"s"},
			Ancestors: []Commit{{SHA: "ccc", Committed: at("2026-11-12T10:00:00Z"), Sessions: []string{"s"}}}}
		got := Explain(c, events)
		if len(got.Sessions) != 1 || got.Sessions[0].Basis != basisTrailer+"ccc" {
			t.Errorf("Explain(ddd) = %+v, want the window to start at its ancestor ccc", got)
		}
	})
}

// Guards: a rebase crediting old work to the moment it was replayed. post-commit records the
// picks a rebase replays; a commit that is the new side of a rebase commit.rewritten line is a
// replay, so its own commit.recorded is neither its record nor anyone's previous commit.
func TestReplayedCommitsAreSkipped(t *testing.T) {
	events := []Event{
		{Event: "commit.recorded", TS: "2026-11-12T10:00:00+00:00", SessionID: "s", Branch: "f", CommitSHA: "aaa", PromptID: "p-1"},
		{Event: "tool.call", TS: "2026-11-12T10:05:00+00:00", SessionID: "s", Tool: "Edit"},
		{Event: "commit.recorded", TS: "2026-11-12T10:10:00+00:00", SessionID: "s", Branch: "f", CommitSHA: "bbb", PromptID: "p-2"},
		{Event: "tool.call", TS: "2026-11-12T10:15:00+00:00", SessionID: "s", Tool: "Bash"},
		{Event: "commit.recorded", TS: "2026-11-12T10:20:00+00:00", SessionID: "s", Branch: "f", CommitSHA: "aaa2", PromptID: "p-3"},
		{Event: "commit.recorded", TS: "2026-11-12T10:20:00+00:00", SessionID: "s", Branch: "f", CommitSHA: "bbb2", PromptID: "p-3"},
		{Event: "commit.rewritten", TS: "2026-11-12T10:20:00+00:00", SessionID: "s", Branch: "f", CommitSHA: "aaa2", OldCommitSHA: "aaa", RewriteKind: "rebase"},
		{Event: "commit.rewritten", TS: "2026-11-12T10:20:00+00:00", SessionID: "s", Branch: "f", CommitSHA: "bbb2", OldCommitSHA: "bbb", RewriteKind: "rebase"},
	}
	got := Explain(Commit{SHA: "bbb2", Committed: at("2026-11-12T10:20:00Z")}, events)
	if len(got.Sessions) != 1 {
		t.Fatalf("Explain(bbb2) = %+v, want one session", got)
	}
	s := got.Sessions[0]
	if s.Basis != basisRecorded+"aaa" || !s.To.Equal(at("2026-11-12T10:10:00Z")) ||
		!reflect.DeepEqual(s.Tools, map[string]int{"Edit": 1}) || !reflect.DeepEqual(s.Prompts, []string{"p-2"}) {
		t.Errorf("Explain(bbb2) = %+v, want bbb's window: after aaa, up to 10:10, Edit 1, prompt p-2", s)
	}
}

// Guards: a replay starting another commit's window through the trailer fallback. A commit
// whose previous commit comes from its first-parent trailers skips ancestors a rebase replayed,
// as previousRecorded does, so work done before the rebase is not cut off.
func TestTrailerFallbackSkipsReplays(t *testing.T) {
	events := []Event{{Event: "commit.rewritten", TS: "2026-11-12T10:20:00+00:00", SessionID: "s", CommitSHA: "r1", OldCommitSHA: "o1", RewriteKind: "rebase"}}
	c := Commit{SHA: "hand", Committed: at("2026-11-12T10:21:00Z"), Sessions: []string{"s"}, Ancestors: []Commit{
		{SHA: "r1", Committed: at("2026-11-12T10:20:00Z"), Sessions: []string{"s"}},
		{SHA: "p", Committed: at("2026-11-12T10:00:00Z"), Sessions: []string{"s"}},
	}}
	got := Explain(c, events)
	if len(got.Sessions) != 1 || got.Sessions[0].Basis != basisTrailer+"p" {
		t.Errorf("Explain(hand) = %+v, want the window to start at p, not the replay r1", got)
	}
}

// Guards: an on-call told what a commit's session did in total but not which prompt did it, or
// told a blocked or failed tool ran clean. Each line belongs to the prompt before it in its
// session; a call's tokens follow its message to the prompt call.detail names.
func TestPerPromptSummary(t *testing.T) {
	const s = "s-1"
	events := []Event{
		{Event: "interaction", TS: "2026-11-12T10:00:00+00:00", SessionID: s, Kind: "prompt", InteractionID: "prompt:a", PermissionMode: "auto"},
		{Event: "call.detail", TS: "2026-11-12T10:00:05+00:00", SessionID: s, MessageID: "m1", PromptInteractionID: "prompt:a"},
		{Event: "usage.api_call", TS: "2026-11-12T10:00:05+00:00", SessionID: s, Model: "claude-opus-5-5", MessageID: "m1", RequestID: "r1", InputTokens: 4, OutputTokens: 30},
		{Event: "usage.api_call", TS: "2026-11-12T10:00:05+00:00", SessionID: s, Model: "claude-opus-5-5", MessageID: "m1", RequestID: "r1", InputTokens: 4, OutputTokens: 30},
		{Event: "tool.outcome", TS: "2026-11-12T10:00:06+00:00", SessionID: s, Tool: "Bash", Outcome: "ran"},
		{Event: "tool.outcome", TS: "2026-11-12T10:00:07+00:00", SessionID: s, Tool: "Bash", Outcome: "blocked", BlockCategory: "classifier_denied"},
		{Event: "interaction", TS: "2026-11-12T10:00:08+00:00", SessionID: s, Kind: "tool_error", InteractionID: "tool_error:t1"},
		{Event: "tool.decision", TS: "2026-11-12T10:00:06+00:00", SessionID: s, Tool: "Bash", Decision: "accept", DecisionSource: "config"},
		{Event: "tool.decision", TS: "2026-11-12T10:00:07+00:00", SessionID: s, Tool: "Bash", Decision: "reject", DecisionSource: "user_reject"},
		{Event: "interaction", TS: "2026-11-12T10:05:00+00:00", SessionID: s, Kind: "prompt", InteractionID: "prompt:b", PermissionMode: "default"},
		// Written late, after prompt:b, but its call.detail names prompt:a.
		{Event: "usage.api_call", TS: "2026-11-12T10:05:01+00:00", SessionID: s, Model: "claude-opus-5-5", MessageID: "m2", RequestID: "r2", OutputTokens: 7},
		{Event: "call.detail", TS: "2026-11-12T10:05:01+00:00", SessionID: s, MessageID: "m2", PromptInteractionID: "prompt:a"},
		{Event: "tool.outcome", TS: "2026-11-12T10:05:02+00:00", SessionID: s, Tool: "Read", Outcome: "ran"},
		{Event: "tool.outcome", TS: "2026-11-12T10:05:03+00:00", SessionID: "s-2", Tool: "Edit", Outcome: "ran"},
	}
	all := func(i int) bool { return events[i].SessionID == s }
	got := summarize(events, all)
	want := []PromptSummary{
		{ID: "prompt:a", At: at("2026-11-12T10:00:00Z"), Mode: "auto", Tools: map[string]int{"Bash": 2},
			Results:   map[string]int{"ran": 1, "blocked:classifier_denied": 1, "tool_error": 1},
			Approvals: map[string]int{"accept:config": 1, "reject:user_reject": 1},
			Models:    map[string]Usage{"claude-opus-5-5": {Calls: 2, Input: 4, Output: 37}}},
		{ID: "prompt:b", At: at("2026-11-12T10:05:00Z"), Mode: "default", Tools: map[string]int{"Read": 1},
			Results: map[string]int{"ran": 1}, Approvals: map[string]int{}, Models: map[string]Usage{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("summarize =\n%+v\nwant\n%+v", got, want)
	}
}

// Guards: a session's tools line that disagrees with its own per-prompt lines (#122). When a
// tool-call backfill ran before the session went on, the earlier prompts have tool.call lines
// and the later ones only tool.outcome; every call still counts once in the session total.
func TestMixedSourceWindowToolsReconcile(t *testing.T) {
	const s = "s-1"
	events := []Event{
		{Event: "interaction", TS: "2026-11-12T10:00:00+00:00", SessionID: s, Kind: "prompt", InteractionID: "prompt:a"},
		{Event: "tool.call", TS: "2026-11-12T10:00:01+00:00", SessionID: s, Tool: "Bash", ToolUseID: "t1", Result: "ok"},
		{Event: "tool.outcome", TS: "2026-11-12T10:00:02+00:00", SessionID: s, Tool: "Bash", ToolUseID: "t1", Outcome: "ran"},
		{Event: "interaction", TS: "2026-11-12T10:05:00+00:00", SessionID: s, Kind: "prompt", InteractionID: "prompt:b"},
		{Event: "tool.outcome", TS: "2026-11-12T10:05:01+00:00", SessionID: s, Tool: "Bash", ToolUseID: "t2", Outcome: "ran"},
		{Event: "tool.outcome", TS: "2026-11-12T10:05:02+00:00", SessionID: s, Tool: "Read", ToolUseID: "t3", Outcome: "ran"},
		{Event: "commit.recorded", TS: "2026-11-12T10:10:00+00:00", SessionID: s, Branch: "f", CommitSHA: "aaa"},
	}
	got := Explain(Commit{SHA: "aaa", Committed: at("2026-11-12T10:10:00Z")}, events)
	want := map[string]int{"Bash": 2, "Read": 1}
	if len(got.Sessions) != 1 || !reflect.DeepEqual(got.Sessions[0].Tools, want) {
		t.Errorf("Explain(aaa) = %+v, want session tools %v (t1, t2, t3 once each)", got, want)
	}
}

// Guards: a session that committed on several branches crediting everything since it started to
// each branch's first commit (one repo's 4113eb2 took 4,863 tool calls and 5,626 model calls going
// back a week). The first commit on a branch takes the work since the branch was checked
// out; with no checkout line, since the session's previous commit on any branch; only with
// neither, the whole session. --json names which (window_basis_kind): it said "other" for the
// first two.
func TestFirstCommitOnABranch(t *testing.T) {
	const s = "s"
	early := Event{Event: "tool.call", TS: "2026-11-12T09:00:00+00:00", SessionID: s, Tool: "Read"}
	other := Event{Event: "commit.recorded", TS: "2026-11-12T09:30:00+00:00", SessionID: s, Repo: "o/r", Branch: "a", CommitSHA: "aaa"}
	mid := Event{Event: "tool.call", TS: "2026-11-12T09:45:00+00:00", SessionID: s, Tool: "Edit"}
	checkout := Event{Event: "work.started", TS: "2026-11-12T09:50:00+00:00", Repo: "o/r", Branch: "b"}
	late := Event{Event: "tool.call", TS: "2026-11-12T09:55:00+00:00", SessionID: s, Tool: "Bash"}
	own := Event{Event: "commit.recorded", TS: "2026-11-12T10:00:00+00:00", SessionID: s, Repo: "o/r", Branch: "b", CommitSHA: "bbb"}
	for _, c := range []struct {
		name   string
		events []Event
		from   string
		tools  map[string]int
		kind   string
	}{
		{"since the branch's checkout", []Event{early, other, mid, checkout, late, own}, "2026-11-12T09:50:00Z", map[string]int{"Bash": 1}, "checkout"},
		{"since the session's previous commit on another branch", []Event{early, other, mid, late, own}, "2026-11-12T09:30:00Z", map[string]int{"Edit": 1, "Bash": 1}, "any_branch"},
		{"the whole session when it made no earlier commit", []Event{early, mid, late, own}, "0001-01-01T00:00:00Z", map[string]int{"Read": 1, "Edit": 1, "Bash": 1}, "none"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Explain(Commit{SHA: "bbb", Committed: at("2026-11-12T10:00:00Z")}, c.events)
			if len(got.Sessions) != 1 || !got.Sessions[0].From.Equal(at(c.from)) || !reflect.DeepEqual(got.Sessions[0].Tools, c.tools) {
				t.Errorf("Explain(bbb) = %+v, want the window from %s with tools %v", got, c.from, c.tools)
				return
			}
			if kind := sessionJSON(got.Sessions[0]).BasisKind; kind != c.kind {
				t.Errorf("window_basis_kind = %q (basis %q), want %q", kind, got.Sessions[0].Basis, c.kind)
			}
		})
	}
}

// Guards: hand-backs and notices counted as a person's prompts. Claude Code's prompt hook also
// fires for a subagent's hand-back, a task notification and a scheduled prompt: livy 2ba3fd4's
// window listed 6 prompts, 4 hand-backs and 2 scheduled prompts. A prompt.submitted line counts
// when extraction wrote a prompt interaction beside it, or has not read that far yet.
func TestOnlyTypedPromptsCount(t *testing.T) {
	t.Parallel()
	const s = "s-1"
	events := []Event{
		{Event: "prompt.submitted", TS: "2026-11-23T05:23:15+00:00", SessionID: s, PromptID: "hand-back"},
		{Event: "prompt.submitted", TS: "2026-11-23T05:25:34+00:00", SessionID: s, PromptID: "typed"},
		{Event: "interaction", TS: "2026-11-23T05:25:35.120Z", SessionID: s, Kind: "prompt", InteractionID: "prompt:x"},
		{Event: "call.detail", TS: "2026-11-23T05:30:00.000Z", SessionID: s, MessageID: "m1"},
		{Event: "prompt.submitted", TS: "2026-11-23T05:33:06+00:00", SessionID: s, PromptID: "not-extracted-yet"},
		{Event: "commit.recorded", TS: "2026-11-23T05:38:19+00:00", SessionID: s, PromptID: "not-extracted-yet", Branch: "f", CommitSHA: "aaa"},
	}
	got := Explain(Commit{SHA: "aaa", Committed: at("2026-11-23T05:38:19Z")}, events)
	want := []string{"typed", "not-extracted-yet"}
	if len(got.Sessions) != 1 || !reflect.DeepEqual(got.Sessions[0].Prompts, want) {
		t.Errorf("Explain(aaa) = %+v, want prompts %v", got, want)
	}
}
