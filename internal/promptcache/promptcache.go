// Package promptcache classifies each model call by how much of the prompt cache it reused,
// from the ledger's usage.api_call lines: the call's cache reads against the prefix the previous
// call on the same cache left behind (its reads plus its writes). A miss carries its cause:
// a compaction (an intended rewrite), an idle gap past the cache's lifetime, a switch of model,
// or none Livy can see.
//
// A cache is one thread: a session's own calls, or one subagent's, on one model. A subagent or a
// call on another model has a cache of its own.
package promptcache

import (
	"cmp"
	"slices"
	"time"

	"github.com/0xbew/livy/internal/why"
)

// Class is how much of the previous prefix a call read back.
type Class string

// The classes. A full miss read back at most fullMissShare of the previous prefix.
const (
	ClassUnknown     Class = ""             // the call has no message or request id to key it by
	ClassFirst       Class = "first"        // no earlier call on its thread left a prefix to reuse
	ClassHit         Class = "hit"          // read back all of the previous prefix it still sent
	ClassPartialMiss Class = "partial_miss" // read back some, not all
	ClassFullMiss    Class = "full_miss"    // read back at most a tenth
)

// Cause is why a call missed.
type Cause string

// The causes, in the order they are tried: the first that applies is the cause.
const (
	CauseNone        Cause = ""
	CauseCompaction  Cause = "compaction"   // the thread compacted since the previous call
	CauseModelSwitch Cause = "model_switch" // the first call on this model, after one on another
	CauseIdle1h      Cause = "idle_past_1h" // the gap passed the 1-hour lifetime it was written with
	CauseIdle5m      Cause = "idle_past_5m" // the gap passed the 5-minute lifetime it was written with
	CauseUnexplained Cause = "unexplained"  // none of the above
)

// fullMissShare is the most of the previous prefix a full miss reads back.
const fullMissShare = 0.10

// Cache lifetimes. A thread's lifetime is the one its latest split cache write used.
const (
	life5m = 5 * time.Minute
	life1h = time.Hour
)

// A mid-burst drop is an unexplained miss under burstGap after the previous call that wrote at
// least dropMinWrite tokens to the cache while reading back under half of the previous prefix.
const (
	burstGap     = 5 * time.Minute
	dropMinWrite = 10_000
)

// Ledger lines read.
const (
	eventUsage  = "usage.api_call"
	eventTurn   = "interaction"
	kindCompact = "compacted"
)

// Key names one call: its API message id and request id.
type Key struct{ Message, Request string }

// Verdict is one call's classification.
type Verdict struct {
	Class Class
	Cause Cause // set only for a miss
	// Prefix is the previous call's reusable prefix (its cache reads plus writes); Read and Write
	// are this call's cache reads and writes; Gap is the time since the previous call.
	Prefix, Read, Write int64
	Gap                 time.Duration
}

// Rewritten is the tokens of the previous prefix a miss wrote again (its own new tokens left
// out); 0 for anything but a miss.
func (v Verdict) Rewritten() int64 {
	if v.Class != ClassPartialMiss && v.Class != ClassFullMiss {
		return 0
	}
	return max(0, min(v.Write, v.Prefix-v.Read))
}

// MidBurstDrop says the call is a cache that collapsed mid-burst: an unexplained miss under five
// minutes after the previous call, writing at least 10,000 tokens to the cache while reading back
// under half of the previous prefix.
func (v Verdict) MidBurstDrop() bool {
	return v.Cause == CauseUnexplained && v.Gap < burstGap && v.Write >= dropMinWrite && 2*v.Read < v.Prefix
}

// call is one usage.api_call line as the classification reads it.
type call struct {
	key                   Key
	session, agent, model string
	at                    time.Time
	read, write           int64
	input                 int64
	split5m, split1h      int64
	split                 bool // the line splits its cache writes by lifetime
}

// Classify is every call's verdict in events, by key. Each call is judged once (a line two
// files hold counts once); a call with neither id is left out.
func Classify(events []why.Event) map[Key]Verdict {
	threads := map[[2]string][]call{}
	compacts := map[[2]string][]time.Time{} // by session and agent: a compaction rewrites its own thread
	seen := map[Key]bool{}
	for _, e := range events {
		if e.Event == eventTurn && e.Kind == kindCompact {
			thread := [2]string{e.SessionID, e.AgentID}
			compacts[thread] = append(compacts[thread], at(e))
			continue
		}
		k := Key{e.MessageID, e.RequestID}
		if e.Event != eventUsage || k == (Key{}) || seen[k] {
			continue
		}
		seen[k] = true
		thread := [2]string{e.SessionID, e.AgentID}
		threads[thread] = append(threads[thread], callOf(e, k))
	}
	out := make(map[Key]Verdict, len(seen))
	for thread, calls := range threads {
		slices.SortStableFunc(calls, func(a, b call) int { return a.at.Compare(b.at) })
		classifyThread(calls, compacts[thread], out)
	}
	return out
}

func callOf(e why.Event, k Key) call {
	c := call{key: k, session: e.SessionID, agent: e.AgentID, model: e.Model, at: at(e),
		read: e.CacheReadTokens, write: e.CacheCreationTokens, input: e.InputTokens}
	if e.CacheCreation5mTokens != nil || e.CacheCreation1hTokens != nil {
		c.split = true
		c.split5m, c.split1h = deref(e.CacheCreation5mTokens), deref(e.CacheCreation1hTokens)
	}
	return c
}

// reference is what a call is judged against: the previous call on its cache, whether that
// call was on another model, and the lifetime its thread last wrote the cache with.
type reference struct {
	prev     call
	switched bool
	lifetime time.Duration
}

// classifyThread judges one thread's calls, oldest first, against the previous call on the same
// model (its own cache), else the thread's previous call on another model (a switch).
func classifyThread(calls []call, compacts []time.Time, out map[Key]Verdict) {
	last := map[string]int{} // each model's latest call, by index
	life := map[string]time.Duration{}
	for i, c := range calls {
		var ref *reference
		if j, ok := last[c.model]; ok {
			ref = &reference{prev: calls[j]}
		} else if i > 0 {
			ref = &reference{prev: calls[i-1], switched: true}
		}
		if ref != nil {
			ref.lifetime = cmp.Or(life[ref.prev.model], life5m)
		}
		out[c.key] = judge(c, ref, compacts)
		last[c.model] = i
		if l := lifetime(c); l != 0 {
			life[c.model] = l
		}
	}
}

// judge is call c's verdict against ref (nil for none).
func judge(c call, ref *reference, compacts []time.Time) Verdict {
	v := Verdict{Class: ClassFirst, Read: c.read, Write: c.write}
	if ref == nil {
		return v
	}
	p := ref.prev
	v.Prefix, v.Gap = p.read+p.write, c.at.Sub(p.at)
	if v.Prefix == 0 {
		return v
	}
	context := c.input + c.read + c.write
	switch {
	case c.read >= min(v.Prefix, context):
		v.Class = ClassHit
		return v
	case float64(c.read) <= fullMissShare*float64(v.Prefix):
		v.Class = ClassFullMiss
	default:
		v.Class = ClassPartialMiss
	}
	v.Cause = cause(c.at, *ref, compacts)
	return v
}

// cause is why a call at now missed the cache ref's call left.
func cause(now time.Time, ref reference, compacts []time.Time) Cause {
	prev := ref.prev.at
	gap := now.Sub(prev)
	compacted := slices.ContainsFunc(compacts, func(t time.Time) bool { return t.After(prev) && !t.After(now) })
	switch {
	case compacted:
		return CauseCompaction
	case ref.switched:
		return CauseModelSwitch // the new model's cache never held this thread's prefix
	case gap >= ref.lifetime && ref.lifetime == life1h:
		return CauseIdle1h
	case gap >= ref.lifetime:
		return CauseIdle5m
	}
	return CauseUnexplained
}

// lifetime is the cache lifetime a call wrote with: 1 hour when any of its writes were, 5
// minutes when they all were; 0 when it wrote nothing it split.
func lifetime(c call) time.Duration {
	switch {
	case !c.split:
		return 0
	case c.split1h > 0:
		return life1h
	case c.split5m > 0:
		return life5m
	}
	return 0
}

func at(e why.Event) time.Time {
	t, _ := why.ParseTS(e.TS)
	return t
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
