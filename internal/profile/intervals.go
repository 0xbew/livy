package profile

import (
	"slices"
	"time"
)

// interval is a span of time, from inclusive to exclusive.
type interval struct{ from, to time.Time }

// intervals is a set of times, as possibly overlapping intervals until union merges them.
type intervals []interval

// clip is s's time inside [from, end]; from may be zero for no lower bound.
func clip(s Span, from, end time.Time) interval {
	return interval{maxTime(s.Start, from), minTime(s.End, end)}
}

// union is the set as sorted, disjoint, non-empty intervals.
func (iv intervals) union() intervals {
	sorted := slices.Clone(iv)
	slices.SortFunc(sorted, func(a, b interval) int { return a.from.Compare(b.from) })
	var out intervals
	for _, i := range sorted {
		if !i.to.After(i.from) {
			continue
		}
		if last := len(out) - 1; last >= 0 && !i.from.After(out[last].to) {
			out[last].to = maxTime(out[last].to, i.to)
			continue
		}
		out = append(out, i)
	}
	return out
}

// minus is the union of iv without the times in cut, which must be a union.
func (iv intervals) minus(cut intervals) intervals {
	var out intervals
	for _, i := range iv.union() {
		cur := i.from
		for _, c := range cut {
			if !c.to.After(cur) || !c.from.Before(i.to) {
				continue
			}
			if c.from.After(cur) {
				out = append(out, interval{cur, c.from})
			}
			cur = maxTime(cur, c.to)
		}
		if cur.Before(i.to) {
			out = append(out, interval{cur, i.to})
		}
	}
	return out
}

// length is the total time in a union.
func (iv intervals) length() time.Duration {
	var d time.Duration
	for _, i := range iv {
		d += i.to.Sub(i.from)
	}
	return d
}
