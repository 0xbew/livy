package promptcache_test

import (
	"testing"

	"github.com/0xbew/livy/internal/promptcache"
	"github.com/0xbew/livy/internal/why"
)

// Guards: a cache rewrite counted as a mid-burst drop when the ledger explains it (a compaction,
// or an idle gap past the cache's lifetime), and a real drop missed. Each row is real: a call and
// the previous call on its cache from this machine's ledger (session 4e7270ba, 2026-11-20 to 22),
// judged by hand from their token counts.
func TestClassify(t *testing.T) {
	t.Parallel()
	const session = "4e7270ba-f9f4-4470-ba5d-9f1d26d94dd5"
	call := func(ts, agent, msg string, input, read, write, w5m, w1h int64) why.Event {
		return why.Event{Event: "usage.api_call", TS: ts, SessionID: session, AgentID: agent, Model: "claude-opus-5-5",
			MessageID: msg, RequestID: "req-" + msg, InputTokens: input, CacheReadTokens: read, CacheCreationTokens: write,
			CacheCreation5mTokens: &w5m, CacheCreation1hTokens: &w1h}
	}
	compacted := why.Event{Event: "interaction", Kind: "compacted", TS: "2026-11-21T00:15:23+00:00", SessionID: session}
	for _, tc := range []struct {
		name      string
		events    []why.Event
		class     promptcache.Class
		cause     promptcache.Cause
		rewritten int64
		drop      bool
	}{
		{"collapsed 179 s after the previous call, nothing to explain it: a drop",
			[]why.Event{call("2026-11-22T05:16:01+00:00", "", "prev", 4, 209709, 737, 0, 737),
				call("2026-11-22T05:19:00+00:00", "", "this", 2, 27701, 191155, 0, 191155)},
			promptcache.ClassPartialMiss, promptcache.CauseUnexplained, 182745, true},
		{"read nothing back after a compaction: the compaction's rewrite",
			[]why.Event{call("2026-11-20T23:20:17+00:00", "", "prev", 4, 332401, 3647, 0, 3647), compacted,
				call("2026-11-21T05:42:36+00:00", "", "this", 2, 0, 112588, 0, 112588)},
			promptcache.ClassFullMiss, promptcache.CauseCompaction, 112588, false},
		{"a subagent's 5-minute cache read nothing back 26 minutes later: expired",
			[]why.Event{call("2026-11-22T04:11:54+00:00", "a8d4fce9fed0077b2", "prev", 2, 102988, 1997, 1997, 0),
				call("2026-11-22T04:38:01+00:00", "a8d4fce9fed0077b2", "this", 4, 0, 107266, 107266, 0)},
			promptcache.ClassFullMiss, promptcache.CauseIdle5m, 104985, false},
		{"the next call reads the rewritten prefix back: a hit",
			[]why.Event{call("2026-11-22T05:19:00+00:00", "", "prev", 2, 27701, 191155, 0, 191155),
				call("2026-11-22T05:19:06+00:00", "", "this", 2, 218856, 790, 0, 790)},
			promptcache.ClassHit, promptcache.CauseNone, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := promptcache.Classify(tc.events)[promptcache.Key{Message: "this", Request: "req-this"}]
			if v.Class != tc.class || v.Cause != tc.cause || v.Rewritten() != tc.rewritten || v.MidBurstDrop() != tc.drop {
				t.Errorf("got %s/%q, %d rewritten, drop %v; want %s/%q, %d rewritten, drop %v",
					v.Class, v.Cause, v.Rewritten(), v.MidBurstDrop(), tc.class, tc.cause, tc.rewritten, tc.drop)
			}
		})
	}
}
