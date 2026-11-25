package profile_test

import (
	"slices"
	"testing"
	"time"

	"github.com/0xbew/livy/internal/profile"
	"github.com/0xbew/livy/internal/why"
)

// TestPromptlessWindowIsTimed guards a Claude Code window with model calls and tool calls but no
// prompt line reading "no prompt and no span: nothing to time": the case when Claude Code runs
// in a folder outside every in-scope repo and its calls work in one (cd, git -C, a file path),
// so no hook line and no prompt interaction is written, only the calls and tools. The fixture is
// hookSession's real ledger lines with the prompt lines taken out; the human's two sleeps
// between its turns (25 s, then 20 s) are known apart from this package, so each wait between
// the inferred turns must be at least as long.
func TestPromptlessWindowIsTimed(t *testing.T) {
	t.Parallel()
	col := why.NewCollector()
	if err := col.File("fixture", "testdata/hook-session.jsonl"); err != nil {
		t.Fatal(err)
	}
	end := at(t, "2026-11-25T15:09:00Z")
	s := why.Session{ID: hookSession, Harness: "claude-code", To: end}
	commits := []why.Explained{{Commit: why.Commit{SHA: "0000000", Committed: end}, Report: why.Report{Sessions: []why.Session{s}}}}
	promptLines := []string{"prompt.submitted", "turn.ended", "interaction"}
	for _, tc := range []struct {
		name string
		drop []string
	}{
		{"hooks record the tools", promptLines},
		{"only the transcript's lines", append(slices.Clone(promptLines), "tool.call", "session.started")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var events []why.Event
			for _, e := range col.Events() {
				if slices.Contains(tc.drop, e.Event) {
					continue
				}
				e.PromptInteractionID = "" // extraction keeps it only for a prompt typed inside a repo
				events = append(events, e)
			}
			got := profile.Report(commits, events, nil, 100, nil).Commits[0].Sessions[0]
			if !got.Timed || got.TimingSource != "hooks" || !got.TurnsInferred {
				t.Fatalf("timed %v from %q, turns inferred %v; want timed from hooks with inferred turns",
					got.Timed, got.TimingSource, got.TurnsInferred)
			}
			checkSleeps(t, got.Waits, []time.Duration{25 * time.Second, 20 * time.Second})
			if got.ModelMS == 0 {
				t.Errorf("model %d ms: want the placed model calls timed", got.ModelMS)
			}
		})
	}
}

// checkSleeps checks each wait between turns long enough to be a person's (over 5 s) is at least
// the sleep the human took there.
func checkSleeps(t *testing.T, waits []profile.JSONWait, sleeps []time.Duration) {
	t.Helper()
	var between []profile.JSONWait
	for _, w := range waits {
		if w.Kind == "between_turns" && w.DurationMS >= 5000 {
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
}
