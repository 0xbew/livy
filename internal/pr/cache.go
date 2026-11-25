package pr

import (
	"math"

	"github.com/0xbew/livy/internal/promptcache"
	"github.com/0xbew/livy/internal/why"
)

// ratePlaces is how many decimal places a hit rate keeps.
const ratePlaces = 1e4

// Cache is how well a set of calls reused the prompt cache.
type Cache struct {
	// HitRate is cache reads over all input tokens (reads, writes and uncached input); null with
	// no input tokens.
	HitRate *float64 `json:"cache_hit_rate"`
	// Drops are the mid-burst drops: unexplained misses under five minutes after the previous
	// call that wrote at least 10,000 tokens to the cache while reading back under half of the
	// previous prefix (promptcache.Verdict.MidBurstDrop).
	Drops Drops `json:"cache_drops_mid_burst"`
	// Classes counts the calls by cache class: first, hit, partial_miss, full_miss, and
	// unclassified (no message or request id to judge it by). Every key is always present.
	Classes map[string]int `json:"cache_classes"`
	// Misses are the misses by cause, each with the tokens of the previous prefix they wrote
	// again. Every cause is always present.
	Misses map[string]Drops `json:"cache_misses"`
}

// Drops are a count of calls and the tokens of the lost prefix they wrote again (each call's own
// new tokens left out).
type Drops struct {
	Count           int   `json:"count"`
	TokensRewritten int64 `json:"tokens_rewritten"`
}

// classes and causes are the keys Cache always carries.
var (
	classes = []promptcache.Class{promptcache.ClassFirst, promptcache.ClassHit, promptcache.ClassPartialMiss,
		promptcache.ClassFullMiss}
	causes = []promptcache.Cause{promptcache.CauseCompaction, promptcache.CauseModelSwitch, promptcache.CauseIdle1h,
		promptcache.CauseIdle5m, promptcache.CauseUnexplained}
)

const unclassified = "unclassified"

// cacheOf is calls' hit rate, classes, misses by cause and mid-burst drops.
func cacheOf(calls []why.Call, verdicts map[promptcache.Key]promptcache.Verdict) Cache {
	out := Cache{Classes: map[string]int{unclassified: 0}, Misses: map[string]Drops{}}
	for _, c := range classes {
		out.Classes[string(c)] = 0
	}
	for _, c := range causes {
		out.Misses[string(c)] = Drops{}
	}
	var read, all int64
	for _, c := range calls {
		read += c.CacheRead
		all += c.CacheRead + c.CacheWrite + c.Input
		v, ok := verdicts[promptcache.Key{Message: c.MessageID, Request: c.RequestID}]
		if !ok || v.Class == promptcache.ClassUnknown {
			out.Classes[unclassified]++
			continue
		}
		out.Classes[string(v.Class)]++
		if v.Cause != promptcache.CauseNone {
			m := out.Misses[string(v.Cause)]
			m.Count++
			m.TokensRewritten += v.Rewritten()
			out.Misses[string(v.Cause)] = m
		}
		if v.MidBurstDrop() {
			out.Drops.Count++
			out.Drops.TokensRewritten += v.Rewritten()
		}
	}
	if all > 0 {
		v := math.Round(float64(read)/float64(all)*ratePlaces) / ratePlaces
		out.HitRate = &v
	}
	return out
}
