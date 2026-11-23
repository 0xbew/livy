package profile

import (
	"strings"
	"testing"
	"time"

	"github.com/0xbew/livy/internal/why"
)

// Guards: `livy profile` saying "no prompt" for a commit whose window `livy why` listed six
// prompts for (livy 2ba3fd4). why takes a window's prompts from prompt.submitted lines;
// profile took them only from extraction's prompt interactions, which that window had none of.
// The lines are prompt.submitted only, the first at 05:23:15, which extraction has not read yet.
func TestProfileTimesFromWhysPrompts(t *testing.T) {
	t.Parallel()
	const s = "s-1"
	events := []why.Event{
		{Event: "prompt.submitted", TS: "2026-11-23T05:23:15+00:00", SessionID: s, PromptID: "p-1"},
		{Event: "tool.outcome", TS: "2026-11-23T05:24:00+00:00", SessionID: s, Tool: "Bash", Outcome: "ran"},
		{Event: "prompt.submitted", TS: "2026-11-23T05:25:34+00:00", SessionID: s, PromptID: "p-2"},
	}
	committed := time.Date(2026, 11, 23, 5, 38, 19, 0, time.UTC)
	c := why.Commit{SHA: "2ba3fd4f653c", Committed: committed, Sessions: []string{s},
		Ancestors: []why.Commit{{SHA: "f6e9c01c27b5", Committed: time.Date(2026, 11, 23, 5, 18, 9, 0, time.UTC), Sessions: []string{s}}}}
	e := why.Explained{Commit: c, Report: why.Explain(c, events)}
	if got := len(e.Report.Sessions[0].Prompts); got != 2 {
		t.Fatalf("why found %d prompts, want 2", got)
	}
	var out strings.Builder
	r := report{w: &out, events: events}
	r.commit(e, "")
	if want := "from the first prompt at 2026-11-23T05:23:15Z"; !strings.Contains(out.String(), want) {
		t.Errorf("profile wrote\n%s\nwant a line saying %q", out.String(), want)
	}
}
