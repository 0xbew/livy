package pr

import (
	"math"
	"slices"
	"time"

	"github.com/0xbew/livy/internal/why"
)

// A mid-burst cache drop is a call, under burstGap after its stream's previous call and with no
// compaction between them, that wrote at least dropMinWrite tokens to the cache while reading
// back under half of the prefix that call had cached (read plus written). Under five minutes even
// a 5-minute cache is still alive, so the rewrite is not an idle gap's expiry; a compaction
// rewrites the prompt on purpose.
const (
	burstGap     = 5 * time.Minute
	dropMinWrite = 10_000
	ratePlaces   = 1e4
	eventTurn    = "interaction"
	kindCompact  = "compacted"
)

// Cache is how well a set of calls reused the prompt cache.
type Cache struct {
	// HitRate is cache reads over all input tokens (reads, writes and uncached input); null with
	// no input tokens.
	HitRate *float64 `json:"cache_hit_rate"`
	Drops   Drops    `json:"cache_drops_mid_burst"`
}

// Drops are the calls whose cache read collapsed mid-burst, and the tokens of the lost prefix they
// wrote again (the new turn's own tokens left out).
type Drops struct {
	Count           int   `json:"count"`
	TokensRewritten int64 `json:"tokens_rewritten"`
}

// stream is the calls that share one cache: a session's own thread or one of its subagents, on
// one model.
type stream struct{ session, agent, model string }

// before is what a drop is judged against: each call's predecessor in its stream (by message
// and request id), and each session's compactions. Both come from every line read, not only
// the windows, so a window's first call has a predecessor too.
type before struct {
	prev     map[[2]string]why.Event
	compacts map[string][]time.Time
}

func priorCalls(events []why.Event) before {
	by := map[stream][]why.Event{}
	seen := map[[2]string]bool{}
	b := before{prev: map[[2]string]why.Event{}, compacts: map[string][]time.Time{}}
	for _, e := range events {
		if e.Event == eventTurn && e.Kind == kindCompact {
			b.compacts[e.SessionID] = append(b.compacts[e.SessionID], at(e))
		}
		key := [2]string{e.MessageID, e.RequestID}
		if e.Event != eventUsage || key == [2]string{} || seen[key] {
			continue
		}
		seen[key] = true
		s := stream{e.SessionID, e.AgentID, e.Model}
		by[s] = append(by[s], e)
	}
	for _, calls := range by {
		slices.SortStableFunc(calls, func(x, y why.Event) int { return at(x).Compare(at(y)) })
		for i := 1; i < len(calls); i++ {
			b.prev[[2]string{calls[i].MessageID, calls[i].RequestID}] = calls[i-1]
		}
	}
	return b
}

func at(e why.Event) time.Time {
	t, _ := why.ParseTS(e.TS)
	return t
}

// cacheOf is calls' hit rate and mid-burst drops.
func cacheOf(calls []why.Call, b before) Cache {
	var read, all int64
	var d Drops
	for _, c := range calls {
		read += c.CacheRead
		all += c.CacheRead + c.CacheWrite + c.Input
		p, ok := b.prev[[2]string{c.MessageID, c.RequestID}]
		if ok && b.dropped(c, p) {
			d.Count++
			d.TokensRewritten += min(c.CacheWrite, p.CacheReadTokens+p.CacheCreationTokens-c.CacheRead)
		}
	}
	out := Cache{Drops: d}
	if all > 0 {
		v := math.Round(float64(read)/float64(all)*ratePlaces) / ratePlaces
		out.HitRate = &v
	}
	return out
}

// dropped says c, the call after p in its stream, rewrote the stream's cache mid-burst.
func (b before) dropped(c why.Call, p why.Event) bool {
	from := at(p)
	gap := c.At.Sub(from)
	prefix := p.CacheReadTokens + p.CacheCreationTokens
	if gap < 0 || gap >= burstGap || c.CacheWrite < dropMinWrite || 2*c.CacheRead >= prefix {
		return false
	}
	compacted := slices.ContainsFunc(b.compacts[p.SessionID], func(t time.Time) bool { return t.After(from) && !t.After(c.At) })
	return !compacted
}
