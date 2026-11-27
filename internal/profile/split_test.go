package profile_test

import (
	"slices"
	"testing"

	"github.com/0xbew/livy/internal/profile"
	"github.com/0xbew/livy/internal/why"
)

// orchestrator is the session behind testdata/orchestrator-window.jsonl: an orchestrating Claude
// Code session's real ledger lines from 15:20 to 15:47 on 2026-11-23 (repo and branch renamed),
// with tool.call lines from the transcript backfill, as the hooks would time them.
const orchestrator = "4e7270ba-f9f4-4470-ba5d-9f1d26d94dd5"

// TestIdleAgentIsNotModelTime guards a window reading "agent 1m12s (model 1m12s, tools 0.0s)"
// while subagents did the work: the session's own agent made no model call from 15:32:56 to
// 15:45:57 (its transcript shows it waiting on a subagent, then on the human), but its next call,
// at 15:46:10, was timed from its last recorded line at 15:27, and every tool ran under some
// model call, so tools read 0. Expected: no model time for the session's agent in the window (the
// commit at 15:37:57, after one at 15:36:45), and the subagents' work shown as theirs.
func TestIdleAgentIsNotModelTime(t *testing.T) {
	t.Parallel()
	col := why.NewCollector()
	if err := col.File("fixture", "testdata/orchestrator-window.jsonl"); err != nil {
		t.Fatal(err)
	}
	s := why.Session{ID: orchestrator, Harness: "claude-code", From: at(t, "2026-11-23T15:36:45Z"),
		To: at(t, "2026-11-23T15:37:57Z")}
	commits := []why.Explained{{Commit: why.Commit{SHA: "0000000", Committed: s.To}, Report: why.Report{Sessions: []why.Session{s}}}}
	for _, tc := range []struct {
		name string
		drop []string
	}{
		{"with its prompt lines", nil},
		{"no prompt line (Claude Code started outside the repo)", []string{"prompt.submitted", "turn.ended"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var events []why.Event
			for _, e := range col.Events() {
				if !slices.Contains(tc.drop, e.Event) {
					events = append(events, e)
				}
			}
			got := profile.Report(commits, events, nil, 5, nil).Commits[0].Sessions[0]
			if !got.Timed {
				t.Fatalf("window not timed")
			}
			sp := got.Split
			if sp.ModelMS != 0 || sp.SubagentMS < 60000 || sp.ToolMS+sp.ModelMS+sp.SubagentMS != got.AgentMS {
				t.Errorf("split %+v of agent %d ms: want no model time and at least 60 s of subagents, summing to agent time",
					sp, got.AgentMS)
			}
		})
	}
}
