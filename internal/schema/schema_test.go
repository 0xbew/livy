package schema

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The rows are schema/EVOLUTION.md's rule in the reference repo: a change may add an
// optional property, add an event name, relax a constraint or edit a description; it
// may not remove a property, change a type, add to required, or tighten a constraint.
const base = `{"required":["ts"],"properties":{
		"ts":{"type":"string","description":"a"},
		"event":{"type":"string","enum":["a","b"]},
		"n":{"type":"integer","minimum":1},
		"label":{"type":"string","maxLength":100},
		"share":{"type":["number","null"]},
		"m":{"type":"object"},
		"scores":{"type":"object","additionalProperties":{"type":"number"}},
		"checks":{"type":"array","items":{"type":"object","required":["check"],"properties":{"check":{"type":"string"},"status":{"type":"string","enum":["ok","fail"]}}}}}}`

func TestAdditiveOnlyRule(t *testing.T) {
	// Guards: a schema change that makes previously written raw events invalid or change meaning.
	rows := []row{
		{"unchanged", base, nil},
		{"description edited", replace(base, `"description":"a"`, `"description":"b"`), nil},
		{"property added", replace(base, `"ts":{`, `"new":{"type":"string"},"ts":{`), nil},
		{"event name added", replace(base, `["a","b"]`, `["a","b","c"]`), nil},
		{"minimum relaxed", replace(base, `"minimum":1`, `"minimum":0`), nil},
		{"property removed", replace(base, `"n":{"type":"integer","minimum":1},`, ``), []string{"properties.n: removed"}},
		{"type changed", replace(base, `"n":{"type":"integer"`, `"n":{"type":"number"`), []string{"properties.n.type: changed"}},
		{"nullable dropped", replace(base, `["number","null"]`, `"number"`), []string{"properties.share.type: changed"}},
		{"newly required", replace(base, `"required":["ts"]`, `"required":["ts","n"]`), []string{"required: n added"}},
		{"event name removed", replace(base, `["a","b"]`, `["a"]`), []string{"properties.event.enum: b removed"}},
		{"minimum raised", replace(base, `"minimum":1`, `"minimum":2`), []string{"properties.n.minimum: raised"}},
		{"maxLength lowered", replace(base, `"maxLength":100`, `"maxLength":40`), []string{"properties.label.maxLength: lowered"}},
		{"nested property removed", replace(base, `"check":{"type":"string"},`, ``),
			[]string{"properties.checks.items.properties.check: removed"}},
		{"nested enum value removed", replace(base, `["ok","fail"]`, `["ok"]`),
			[]string{"properties.checks.items.properties.status.enum: fail removed"}},
	}
	check(t, rows)
}

func replace(s, old, new string) string {
	if !strings.Contains(s, old) {
		panic("test row edits text the base schema does not contain: " + old)
	}
	return strings.Replace(s, old, new, 1)
}

// A constraint where the base had none is a tightening too: events written without it
// may break it. Any keyword the check has no rule for counts as a change.
func TestAdditiveFlagsConstraintsAddedWhereNoneWere(t *testing.T) {
	// Guards: an enum, pattern or value schema added to an existing field passing as additive.
	check(t, []row{
		{"enum added to a free field", replace(base, `"label":{"type":"string"`, `"label":{"type":"string","enum":["a"]`),
			[]string{"properties.label.enum: added"}},
		{"pattern added", replace(base, `"label":{"type":"string"`, `"label":{"type":"string","pattern":"^a"`),
			[]string{"properties.label.pattern: added"}},
		{"value schema added to a bare object", replace(base, `"m":{"type":"object"}`, `"m":{"type":"object","additionalProperties":{"type":"number"}}`),
			[]string{"properties.m.additionalProperties: tightened"}},
		{"examples added", replace(base, `"label":{"type":"string"`, `"label":{"type":"string","examples":["a"]`), nil},
	})
}

// A key an open object let through its value schema may already be stored under that
// schema, so naming it under properties must not narrow what it allowed.
func TestAdditiveHoldsANamedKeyToTheOpenValueSchema(t *testing.T) {
	// Guards: scores.q0 gaining a minimum or a new type passing as additive while stored
	// lines hold {"q0": 0}.
	check(t, []row{
		{"named key tighter than the value schema", replace(base, `"scores":{"type":"object",`, `"scores":{"type":"object","properties":{"q0":{"type":"number","minimum":1}},`),
			[]string{"properties.scores.properties.q0.minimum: added"}},
		{"named key retyped against the value schema", replace(base, `"scores":{"type":"object",`, `"scores":{"type":"object","properties":{"q0":{"type":"string"}},`),
			[]string{"properties.scores.properties.q0.type: changed"}},
		{"named key as loose as the value schema", replace(base, `"scores":{"type":"object",`, `"scores":{"type":"object","properties":{"q0":{"type":"number","description":"first"}},`), nil},
	})
}

type row struct {
	name string
	head string
	want []string
}

func check(t *testing.T, rows []row) {
	t.Helper()
	for _, row := range rows {
		got, err := Additive([]byte(base), []byte(row.head))
		if err != nil {
			t.Errorf("%s: %v", row.name, err)
			continue
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, row.want) {
			t.Errorf("%s: got %q, want %q", row.name, got, row.want)
		}
	}
}
