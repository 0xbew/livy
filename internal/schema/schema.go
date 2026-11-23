// Package schema checks that a change to the ledger event schema is additive only:
// nothing removed or retyped, nothing newly required, no constraint tightened.
package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// Additive lists every way head breaks the additive-only rule against base, by JSON path.
func Additive(base, head []byte) ([]string, error) {
	var b, h map[string]any
	if err := json.Unmarshal(base, &b); err != nil {
		return nil, fmt.Errorf("base schema: %w", err)
	}
	if err := json.Unmarshal(head, &h); err != nil {
		return nil, fmt.Errorf("head schema: %w", err)
	}
	var out []string
	compare("", b, h, &out)
	return out, nil
}

func join(path, seg string) string {
	if path == "" {
		return seg
	}
	return path + "." + seg
}

func compare(path string, b, h map[string]any, out *[]string) {
	c := comparison{path: path, b: b, h: h, out: out}
	c.typeAndEnum()
	c.unknownKeywords()
	c.required()
	c.bounds()
	c.properties()
	c.nested()
}

// comparison is one schema node of base (b) and head (h) at path, and where breaks go.
type comparison struct {
	path string
	b, h map[string]any
	out  *[]string
}

func (c comparison) add(seg, what string) { *c.out = append(*c.out, join(c.path, seg)+": "+what) }

// typeAndEnum flags a changed type, an enum added, and an enum value removed.
func (c comparison) typeAndEnum() {
	if bt, ok := c.b["type"]; ok && !reflect.DeepEqual(bt, c.h["type"]) {
		c.add("type", "changed")
	}
	he, ok := c.h["enum"].([]any)
	if !ok {
		return
	}
	be, ok := c.b["enum"].([]any)
	if !ok {
		c.add("enum", "added")
		return
	}
	for _, v := range be {
		if !contains(he, v) {
			c.add("enum", fmt.Sprint(v)+" removed")
		}
	}
}

// unknownKeywords flags a keyword this check has no rule for, added or changed: it may
// constrain, so a person decides. Annotations never constrain.
func (c comparison) unknownKeywords() {
	for key, hv := range c.h {
		if handled[key] || annotation[key] {
			continue
		}
		if bv, ok := c.b[key]; !ok {
			c.add(key, "added")
		} else if !reflect.DeepEqual(bv, hv) {
			c.add(key, "changed")
		}
	}
}

// required flags each newly required property.
func (c comparison) required() {
	br, _ := c.b["required"].([]any)
	hr, _ := c.h["required"].([]any)
	for _, r := range hr {
		if !contains(br, r) {
			c.add("required", fmt.Sprint(r)+" added")
		}
	}
}

// bounds flags a bound the base did not have, or a tighter one: either invalidates events
// written before.
func (c comparison) bounds() {
	tighter := map[string]func(b, h float64) bool{
		"minimum":   func(b, h float64) bool { return h > b },
		"minLength": func(b, h float64) bool { return h > b },
		"maximum":   func(b, h float64) bool { return h < b },
		"maxLength": func(b, h float64) bool { return h < b },
	}
	word := map[string]string{"minimum": "raised", "minLength": "raised", "maximum": "lowered", "maxLength": "lowered"}
	for _, key := range []string{"minimum", "minLength", "maximum", "maxLength"} {
		hv, hok := c.h[key].(float64)
		if !hok {
			continue
		}
		bv, bok := c.b[key].(float64)
		if !bok {
			c.add(key, "added")
		} else if tighter[key](bv, hv) {
			c.add(key, word[key])
		}
	}
}

// properties compares each property, flags a removed one, and holds a newly named key to
// the value schema the base let it through.
func (c comparison) properties() {
	bp, _ := c.b["properties"].(map[string]any)
	hp, _ := c.h["properties"].(map[string]any)
	if bap, ok := c.b["additionalProperties"].(map[string]any); ok {
		for name, hv := range hp {
			if _, known := bp[name]; !known {
				hm, _ := hv.(map[string]any)
				compare(join(c.path, "properties."+name), bap, hm, c.out)
			}
		}
	}
	for name, bv := range bp {
		hv, ok := hp[name]
		if !ok {
			c.add("properties."+name, "removed")
			continue
		}
		bm, _ := bv.(map[string]any)
		hm, _ := hv.(map[string]any)
		compare(join(c.path, "properties."+name), bm, hm, c.out)
	}
}

// nested compares items and additionalProperties schemas, and flags either tightened.
func (c comparison) nested() {
	for _, key := range []string{"items", "additionalProperties"} {
		bm, bIsMap := c.b[key].(map[string]any)
		hm, hIsMap := c.h[key].(map[string]any)
		switch {
		case bIsMap && hIsMap:
			compare(join(c.path, key), bm, hm, c.out)
		case c.h[key] == false && c.b[key] != false, hIsMap && !bIsMap && c.b[key] != false:
			c.add(key, "tightened")
		}
	}
}

var (
	handled = map[string]bool{"type": true, "enum": true, "required": true, "properties": true, "items": true,
		"additionalProperties": true, "minimum": true, "minLength": true, "maximum": true, "maxLength": true}
	annotation = map[string]bool{"description": true, "$comment": true, "$schema": true, "$id": true,
		"title": true, "examples": true, "default": true, "deprecated": true}
)

func contains(list []any, v any) bool {
	for _, x := range list {
		if reflect.DeepEqual(x, v) {
			return true
		}
	}
	return false
}
