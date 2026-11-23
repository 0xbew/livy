package profile_test

import (
	"maps"
	"testing"
	"time"

	"github.com/0xbew/livy/internal/profile"
)

// The fixture is real: the spans Livy's receiver stored for one Claude Code session (two
// prompts, three commits, one subagent) and one Codex turn on 2026-11-16, cut to those two
// sessions and the attributes profile reads. The expected values come from a separate
// reading of the same spans (interval unions in Python), not from this package.
const (
	claudeSession = "4de04405-0389-473a-a3b3-c8218e19f63e"
	codexSession  = "01a1e4bc-349d-78a0-b5a6-7bd3716165ea"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func tools(nodes []*profile.Node, into map[string]int) map[string]int {
	for _, n := range nodes {
		if n.Span.Kind() == profile.KindTool {
			into[n.Span.Label()]++
		}
		tools(n.Children, into)
	}
	return into
}

func TestBuild(t *testing.T) {
	t.Parallel()
	spans, bad, err := profile.ReadSpansFile("testdata/otel-traces.jsonl.gz")
	if err != nil || bad != 0 {
		t.Fatalf("ReadSpansFile: %d bad lines, %v", bad, err)
	}
	cases := []struct {
		name                    string
		w                       profile.Window
		wall, busy, model, wait time.Duration
		turns, placed, total    int
		tools                   map[string]int
	}{
		{
			// Guards: a subagent's model calls run inside its Agent tool call; summing spans
			// counts that time twice and agent time comes out above wall time. Also: a tool's
			// approval wait overlaps the model call still streaming; subtracting every wait
			// from all model time cut 39 ms of it.
			name: "a subagent's time is counted once",
			w: profile.Window{Session: claudeSession, End: at(t, "2026-11-16T14:20:39Z"),
				Prompts: []time.Time{at(t, "2026-11-16T14:20:26Z")}},
			wall: 13 * time.Second, busy: 12437581793, model: 11960665707, wait: 537394333,
			turns: 1, placed: 24, total: 25,
			tools: map[string]int{"subagent (Agent)": 1, "tool Bash": 2, "tool Read": 1, "tool Write": 2},
		},
		{
			// Guards: a commit made mid-turn (the turn's second commit) must not take the turn's
			// earlier tool calls, which built the previous commit.
			name: "a commit made mid-turn starts at the previous commit",
			w: profile.Window{Session: claudeSession, From: at(t, "2026-11-16T14:20:52Z"),
				End: at(t, "2026-11-16T14:20:56Z")},
			wall: 4 * time.Second, busy: 3941365625, model: 3487000000, wait: 0,
			turns: 1, placed: 4, total: 4,
			tools: map[string]int{"tool Bash": 1},
		},
		{
			// Guards: only Codex's turn span names the session; its model calls and tools do not,
			// and a profile that joins spans by attribute alone finds none of them. Also: Codex's
			// process-long spans (codex.exec, session_loop) name the session too, and must not
			// move the start before the turn.
			name: "a Codex turn's spans join its session through the turn",
			w:    profile.Window{Session: codexSession, End: at(t, "2026-11-16T14:21:50Z")},
			wall: 7147526000, busy: 6177505000, model: 6060926000, wait: 0,
			turns: 1, placed: 304, total: 308,
			tools: map[string]int{"tool exec_command": 1},
		},
		{
			// Guards: a session-level span (Codex's session_loop) running over an idle gap after
			// the previous commit read as "a turn was running" and pulled that gap into the window.
			name: "a session-level span does not start the window at the previous commit",
			w: profile.Window{Session: codexSession, From: time.Unix(0, 1794838902172420000).UTC(),
				End: at(t, "2026-11-16T14:21:50Z")},
			wall: 7147526000, busy: 6177505000, model: 6060926000, wait: 0,
			turns: 1, placed: 304, total: 308,
			tools: map[string]int{"tool exec_command": 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := profile.Build(spans, tc.w)
			got := []time.Duration{p.Wall, p.Busy, p.Model, p.Wait}
			want := []time.Duration{tc.wall, tc.busy, tc.model, tc.wait}
			if got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
				t.Errorf("wall, busy, model, wait = %v, want %v", got, want)
			}
			if len(p.Turns) != tc.turns || p.Placed != tc.placed || p.Total != tc.total {
				t.Errorf("turns %d, placed %d of %d; want %d, %d of %d", len(p.Turns), p.Placed, p.Total,
					tc.turns, tc.placed, tc.total)
			}
			if gt := tools(p.Turns, map[string]int{}); !maps.Equal(gt, tc.tools) {
				t.Errorf("tools in the tree = %v, want %v", gt, tc.tools)
			}
		})
	}
}
