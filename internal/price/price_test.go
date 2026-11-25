package price_test

import (
	"math"
	"testing"

	"github.com/0xbew/livy/internal/price"
)

// TestEveryModelInUseIsPriced guards a PR's estimate leaving out the calls to an Anthropic model
// still in use: "150 calls unpriced (model not in the price table: claude-opus-5)" on a machine
// whose sessions ran Opus 5. Each model id is one Claude Code or Amp records (the API's own id,
// dated or not, as https://platform.claude.com/docs/en/about-claude/model-deprecations lists
// them); each want is that model's input plus output list price per million tokens on
// https://platform.claude.com/docs/en/about-claude/pricing, read 2026-11-25.
func TestEveryModelInUseIsPriced(t *testing.T) {
	t.Parallel()
	table, err := price.Load()
	if err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]float64{
		"claude-fable-5-1": 60, "claude-fable-5": 60, "claude-mythos-5-2": 48, "claude-mythos-5-1": 60, "claude-mythos-5": 60,
		"claude-opus-5": 30, "claude-opus-4-8": 30, "claude-opus-4-7": 30, "claude-opus-4-6": 30,
		"claude-opus-4-5": 30, "claude-opus-4-5-20251101": 30, "claude-opus-4-1-20250805": 90, "claude-opus-4-20250514": 90,
		"claude-sonnet-5": 12, "claude-sonnet-4-6": 18, "claude-sonnet-4-5": 18, "claude-sonnet-4-5-20250929": 18,
		"claude-sonnet-4-20250514": 18, "claude-haiku-4-5-20251001": 6,
	} {
		// A tenth of a million each way: under every model's 200K context.
		got, why := table.Cost(price.Call{Model: model, Input: 100_000, Output: 100_000})
		if why != "" || math.Abs(got-want/10) > 1e-9 {
			t.Errorf("Cost(%s, 100K in + 100K out) = %v (unpriced: %q), want $%v", model, got, why, want/10)
		}
	}
	// The pricing page gives Sonnet 4.5 and 4 no rate for a prompt over their 200K context: a
	// longer one (the 1M beta) is unpriced, not priced at the base rate.
	if got, why := table.Cost(price.Call{Model: "claude-sonnet-4-5", Input: 200_001}); why == "" {
		t.Errorf("Cost(claude-sonnet-4-5, 200,001 tokens in) = %v, want unpriced", got)
	}
}
