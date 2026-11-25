// Package price is the vendors' API list prices, one table for every harness Livy reads
// (prices.json, each row with its source and the date it was read), and what one API call
// costs at them. A call the table cannot price is said to be unpriced, never priced at $0.
package price

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sync"
)

//go:embed prices.json
var pricesJSON []byte

// perToken turns a per-million-tokens rate into a per-token one.
const perToken = 1e6

// tierStandard is the service tier and speed the table's rates are for.
const tierStandard = "standard"

// Rates are one model's prices in USD per million tokens.
type Rates struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read"`
	CacheWrite5m float64 `json:"cache_write_5m"`
	CacheWrite1h float64 `json:"cache_write_1h"`
}

// LongPrompt is a model's higher rates for a call whose prompt (input, cache reads and cache
// writes together) is over OverTokens.
type LongPrompt struct {
	OverTokens int64 `json:"over_tokens"`
	Rates
}

// Model is one row of the table.
type Model struct {
	Model string `json:"model"`
	Rates
	LongPrompt *LongPrompt `json:"long_prompt"`
	Source     string      `json:"source"`
	AsOf       string      `json:"as_of"`
}

// Table is the price table.
type Table struct {
	Schema string  `json:"schema"`
	Models []Model `json:"models"`
}

// Call is one API call's tokens and what decides its price. Input excludes the cache reads and
// writes. CacheWriteUnsplit is cache writes whose lifetime the call does not say: priced at the
// 5-minute rate.
type Call struct {
	Model, ServiceTier, Speed  string
	Input, Output, CacheRead   int64
	CacheWrite5m, CacheWrite1h int64
	CacheWriteUnsplit          int64
}

// Unpriced is why a call has no price; "" when it has one.
type Unpriced string

// Why a call is unpriced.
const (
	UnpricedModel Unpriced = "model not in the price table"
	UnpricedTier  Unpriced = "service tier other than standard"
	UnpricedSpeed Unpriced = "speed other than standard"
)

// Load is the table built into the binary, checked: every row names a model once, has a source
// and a date, and no rate is negative or not a number.
var Load = sync.OnceValues(func() (Table, error) { return parse(pricesJSON) })

func parse(data []byte) (Table, error) {
	var t Table
	if err := json.Unmarshal(data, &t); err != nil {
		return Table{}, fmt.Errorf("read the price table: %w", err)
	}
	seen := map[string]bool{}
	for _, m := range t.Models {
		if m.Model == "" || seen[m.Model] || m.Source == "" || m.AsOf == "" {
			return Table{}, fmt.Errorf("price table row %q: needs a model named once, a source and an as_of date", m.Model)
		}
		seen[m.Model] = true
		rates := []Rates{m.Rates}
		if m.LongPrompt != nil {
			rates = append(rates, m.LongPrompt.Rates)
		}
		for _, r := range rates {
			for _, v := range []float64{r.Input, r.Output, r.CacheRead, r.CacheWrite5m, r.CacheWrite1h} {
				if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
					return Table{}, fmt.Errorf("price table row %q: rate %v is not a price", m.Model, v)
				}
			}
		}
	}
	return t, nil
}

// Cost is c's price in USD at the table's rates, or why it has none.
func (t Table) Cost(c Call) (float64, Unpriced) {
	switch {
	case c.ServiceTier != "" && c.ServiceTier != tierStandard:
		return 0, UnpricedTier
	case c.Speed != "" && c.Speed != tierStandard:
		return 0, UnpricedSpeed
	}
	i := slices.IndexFunc(t.Models, func(m Model) bool { return m.Model == c.Model })
	if i < 0 {
		return 0, UnpricedModel
	}
	m := t.Models[i]
	r := m.Rates
	prompt := c.Input + c.CacheRead + c.CacheWrite5m + c.CacheWrite1h + c.CacheWriteUnsplit
	if m.LongPrompt != nil && prompt > m.LongPrompt.OverTokens {
		r = m.LongPrompt.Rates
	}
	usd := float64(c.Input)*r.Input + float64(c.Output)*r.Output + float64(c.CacheRead)*r.CacheRead +
		float64(c.CacheWrite5m+c.CacheWriteUnsplit)*r.CacheWrite5m + float64(c.CacheWrite1h)*r.CacheWrite1h
	return usd / perToken, ""
}

// AsOf is the oldest and newest date the table's rows were read.
func (t Table) AsOf() (oldest, newest string) {
	for _, m := range t.Models {
		if oldest == "" || m.AsOf < oldest {
			oldest = m.AsOf
		}
		if m.AsOf > newest {
			newest = m.AsOf
		}
	}
	return oldest, newest
}
