package pr

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/0xbew/livy/internal/price"
	"github.com/0xbew/livy/internal/promptcache"
	"github.com/0xbew/livy/internal/why"
)

// usdPlaces is how many decimal places an estimate keeps: a millionth of a dollar.
const usdPlaces = 1e6

// Cost is the PR's estimated API cost at list prices, over every call in its commits' windows
// (each call once), and each commit's.
type Cost struct {
	Estimate
	Cache
	Commits []CommitCost `json:"commits"`
}

// CommitCost is one commit's estimate, over the calls in its sessions' windows.
type CommitCost struct {
	SHA string `json:"sha"`
	Estimate
	Cache
}

// Estimate is what a set of calls costs at the price table's rates. EstimatedUSD is the priced
// calls' cost, null when no call was priced: no calls, or none the table prices. Unpriced calls
// are counted and their models named, never priced at $0. Basis says how it was reached.
type Estimate struct {
	EstimatedUSD   *float64 `json:"estimated_usd"`
	Calls          int      `json:"calls"`
	UnpricedCalls  int      `json:"unpriced_calls"`
	UnpricedModels []string `json:"unpriced_models"`
	Basis          string   `json:"basis"`
}

// costOf is the PR's cost and cache use and each commit's, from the calls in the explained
// windows; events are every line read, for each call's predecessor in its stream.
func costOf(explained []why.Explained, events []why.Event) Cost {
	table, err := price.Load()
	verdicts := promptcache.Classify(events)
	out := Cost{Commits: make([]CommitCost, 0, len(explained))}
	var all []why.Call
	for _, ex := range explained {
		var calls []why.Call
		for _, s := range ex.Report.Sessions {
			calls = append(calls, s.Calls...)
		}
		all = append(all, calls...)
		out.Commits = append(out.Commits, CommitCost{SHA: ex.Commit.SHA, Estimate: estimate(calls, table, err),
			Cache: cacheOf(calls, verdicts)})
	}
	all = once(all)
	out.Estimate, out.Cache = estimate(all, table, err), cacheOf(all, verdicts)
	return out
}

// once is calls with a call two windows hold (same message and request id) kept once.
func once(calls []why.Call) []why.Call {
	seen := map[[2]string]bool{}
	return slices.DeleteFunc(slices.Clone(calls), func(c why.Call) bool {
		key := [2]string{c.MessageID, c.RequestID}
		if key == [2]string{} {
			return false
		}
		dup := seen[key]
		seen[key] = true
		return dup
	})
}

// estimate prices calls at table; tableErr is why the table could not be read, and then no
// call is priced.
func estimate(calls []why.Call, table price.Table, tableErr error) Estimate {
	e := Estimate{Calls: len(calls), UnpricedModels: []string{}}
	if len(calls) == 0 {
		e.Basis = "no API calls recorded in the window"
		return e
	}
	var usd float64
	at5m := 0
	reasons := map[price.Unpriced]bool{}
	for _, c := range calls {
		pc := priceCall(c)
		v, reason := table.Cost(pc)
		if tableErr != nil {
			reason = price.Unpriced(fmt.Sprintf("price table unreadable (%v)", tableErr))
		}
		if reason != "" {
			e.UnpricedCalls++
			reasons[reason] = true
			if name := cmp.Or(c.Model, "(no model)"); !slices.Contains(e.UnpricedModels, name) {
				e.UnpricedModels = append(e.UnpricedModels, name)
			}
			continue
		}
		usd += v
		if pc.CacheWriteUnsplit > 0 {
			at5m++
		}
	}
	slices.Sort(e.UnpricedModels)
	if e.UnpricedCalls < e.Calls {
		v := math.Round(usd*usdPlaces) / usdPlaces
		e.EstimatedUSD = &v
	}
	e.Basis = basis(table, e, at5m, reasons)
	return e
}

// priceCall is a ledger call as the price table reads it. Cache writes the line does not split by
// lifetime, and any part of the total its split (or the half of it the line has) leaves out, are
// priced at the 5-minute rate.
func priceCall(c why.Call) price.Call {
	pc := price.Call{Model: c.Model, ServiceTier: c.ServiceTier, Speed: c.Speed, Input: c.Input, Output: c.Output,
		CacheRead: c.CacheRead, CacheWriteUnsplit: c.CacheWrite}
	if c.CacheWrite5m != nil || c.CacheWrite1h != nil {
		if c.CacheWrite5m != nil {
			pc.CacheWrite5m = *c.CacheWrite5m
		}
		if c.CacheWrite1h != nil {
			pc.CacheWrite1h = *c.CacheWrite1h
		}
		pc.CacheWriteUnsplit = max(0, c.CacheWrite-pc.CacheWrite5m-pc.CacheWrite1h)
	}
	return pc
}

// basis says, for people, how e was reached: the table's date, how many calls were priced, how
// many of those had their cache writes priced at the 5-minute rate for want of a split, and
// why the rest were not priced.
func basis(table price.Table, e Estimate, at5m int, reasons map[price.Unpriced]bool) string {
	oldest, newest := table.AsOf()
	when := oldest
	if newest != oldest {
		when = oldest + " to " + newest
	}
	priced := e.Calls - e.UnpricedCalls
	parts := []string{fmt.Sprintf("%d of %d calls priced", priced, e.Calls)}
	if when != "" {
		parts[0] = fmt.Sprintf("API list prices as of %s, standard tier and speed; ", when) + parts[0]
	}
	if at5m > 0 {
		parts = append(parts, fmt.Sprintf("%d calls' cache writes not split into 5-minute and 1-hour, priced at the 5-minute rate", at5m))
	}
	if e.UnpricedCalls > 0 {
		said := make([]string, 0, len(reasons))
		for r := range reasons {
			said = append(said, string(r))
		}
		slices.Sort(said)
		parts = append(parts, fmt.Sprintf("%d calls unpriced (%s: %s), not counted", e.UnpricedCalls,
			strings.Join(said, ", "), strings.Join(e.UnpricedModels, ", ")))
	}
	return strings.Join(parts, "; ")
}
