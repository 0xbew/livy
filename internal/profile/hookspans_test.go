package profile_test

import (
	"testing"
	"time"

	"github.com/0xbew/livy/internal/profile"
	"github.com/0xbew/livy/internal/why"
)

// The fixture is real: Livy's ledger lines for one `claude -p` session on 2026-11-25 (three
// prompts sent 25 s and 20 s after the previous turn ended, one subagent), with OTEL off in
// effect: no span reached Livy. The two sleeps were run by hand between the prompts, so they
// are known apart from anything this package computes.
const hookSession = "abf03e8f-6279-4243-825a-2328c3bf5eae"

// TestHookTimedWindow guards a window with no OTEL span reading as untimed ("agent and waiting
// time unknown") although Livy's own hooks recorded every turn, tool call and model call: the
// case on a machine whose managed settings send Claude Code's OTEL elsewhere.
func TestHookTimedWindow(t *testing.T) {
	t.Parallel()
	col := why.NewCollector()
	if err := col.File("fixture", "testdata/hook-session.jsonl"); err != nil {
		t.Fatal(err)
	}
	prompts := []time.Time{at(t, "2026-11-25T15:07:34Z"), at(t, "2026-11-25T15:08:14Z"), at(t, "2026-11-25T15:08:55Z")}
	end := at(t, "2026-11-25T15:09:00Z") // the last turn.ended
	s := why.Session{ID: hookSession, Harness: "claude-code", To: end, Prompts: []string{"a", "b", "c"}, Sent: prompts}
	commits := []why.Explained{{Commit: why.Commit{SHA: "0000000", Committed: end}, Report: why.Report{Sessions: []why.Session{s}}}}

	got := profile.Report(commits, col.Events(), nil, 100).Commits[0].Sessions[0]

	if !got.Timed || got.TimingSource != "hooks" {
		t.Fatalf("timed %v from %q, want timed from hooks", got.Timed, got.TimingSource)
	}
	sleeps := []time.Duration{25 * time.Second, 20 * time.Second}
	var between []profile.JSONWait
	for _, w := range got.Waits {
		if w.Kind == "between_turns" && w.DurationMS >= 1000 {
			between = append(between, w)
		}
	}
	if len(between) != len(sleeps) {
		t.Fatalf("waits between turns %+v, want one per sleep %v", between, sleeps)
	}
	for i, w := range between {
		if d := time.Duration(w.DurationMS) * time.Millisecond; d < sleeps[i] {
			t.Errorf("wait %d is %v, shorter than the %v the human slept", i, d, sleeps[i])
		}
	}
	if got.ModelMS == 0 || got.AgentMS <= got.ModelMS {
		t.Errorf("agent %d ms, model %d ms: want model calls and tool time both seen", got.AgentMS, got.ModelMS)
	}
	const subagent = "a16ff32003bd0a209" // the id the Agent call's own result names
	launched := false
	for _, sp := range got.Longest {
		if sp.Kind == "subagent" && sp.StartsAgent == subagent {
			launched = true
		}
	}
	if !launched {
		t.Errorf("no subagent span runs %s: its calls would draw outside the call that launched them", subagent)
	}
}
