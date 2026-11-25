package pr

import (
	"slices"
	"testing"

	"github.com/0xbew/livy/internal/price"
	"github.com/0xbew/livy/internal/why"
)

// Guards: a PR's estimate reading lower than its real cost because calls to a model the price
// table lacks were priced at $0. They must count as unpriced and leave the estimate unknown
// when nothing else is priced. Expected dollars are Anthropic's list price for Opus 5.5 input
// ($4 per million tokens).
func TestEstimateNeverPricesUnpricedAtZero(t *testing.T) {
	table, err := price.Load()
	if err != nil {
		t.Fatal(err)
	}
	unknown := why.Call{Model: "gpt-unlisted", Input: 1_000_000, Output: 1_000_000}
	opus := why.Call{Model: "claude-opus-5-5", Input: 1_000_000}
	four := 4.0
	for _, tc := range []struct {
		name     string
		calls    []why.Call
		usd      *float64
		unpriced int
	}{
		{"only an unpriced model", []why.Call{unknown}, nil, 1},
		{"a priced and an unpriced model", []why.Call{opus, unknown}, &four, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := estimate(tc.calls, table, nil)
			if !equalUSD(got.EstimatedUSD, tc.usd) || got.UnpricedCalls != tc.unpriced ||
				!slices.Equal(got.UnpricedModels, []string{"gpt-unlisted"}) {
				t.Errorf("estimate = usd %v, unpriced %d %v; want usd %v, unpriced %d [gpt-unlisted]",
					deref(got.EstimatedUSD), got.UnpricedCalls, got.UnpricedModels, deref(tc.usd), tc.unpriced)
			}
		})
	}
}

func equalUSD(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
