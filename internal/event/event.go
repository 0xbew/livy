// Package event is the ledger event: one struct generated from schema/ledger-event.schema.json,
// every field optional, decoded strictly and encoded byte-equal to the Python ledger.
package event

//go:generate go run ./gen ../../schema/ledger-event.schema.json event_gen.go

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/0xbew/livy/internal/ledgerjson"
)

// Number is a JSON number kept as its literal, so an integer written into a number field
// stays an integer and a float keeps Python's repr.
type Number = json.Number

// Nullable is a field that can be absent, null, or a value.
type Nullable[T any] struct {
	Set  bool // present in the line
	Null bool // present as null
	V    T
}

// Decode parses one ledger line. It refuses what the schema forbids: an unknown field, a
// value of the wrong type, null where the field is not nullable, a missing required field.
func Decode(line []byte) (Event, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var top any
	if err := dec.Decode(&top); err != nil {
		return Event{}, fmt.Errorf("event: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Event{}, errors.New("event: data after the object")
	}
	m, ok := top.(map[string]any)
	if !ok {
		return Event{}, errors.New("event: not a JSON object")
	}
	var e Event
	for k, v := range m {
		if err := e.set(k, v); err != nil {
			return Event{}, fmt.Errorf("event: %s: %w", k, err)
		}
	}
	for _, r := range required {
		if _, ok := m[r]; !ok {
			return Event{}, fmt.Errorf("event: required field %s missing", r)
		}
	}
	return e, nil
}

// Encode writes the event as the Python ledger writes a line (without the newline).
func (e Event) Encode() ([]byte, error) {
	return ledgerjson.Marshal(e.fields())
}

func asString(v any) (*string, error) {
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("want a string, got %s", kind(v))
	}
	return &s, nil
}

func asBool(v any) (*bool, error) {
	b, ok := v.(bool)
	if !ok {
		return nil, fmt.Errorf("want a boolean, got %s", kind(v))
	}
	return &b, nil
}

func asNumber(v any) (*Number, error) {
	n, ok := v.(json.Number)
	if !ok {
		return nil, fmt.Errorf("want a number, got %s", kind(v))
	}
	return &n, nil
}

func asInt(v any) (*int64, error) {
	n, ok := v.(json.Number)
	if !ok || strings.ContainsAny(string(n), ".eE") {
		return nil, fmt.Errorf("want an integer, got %s", kind(v))
	}
	i, err := strconv.ParseInt(string(n), 10, 64)
	if err != nil {
		return nil, err
	}
	return &i, nil
}

func asStrings(v any) (*[]string, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("want an array, got %s", kind(v))
	}
	out := make([]string, 0, len(list))
	for i, x := range list {
		s, err := asString(x)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		out = append(out, *s)
	}
	return &out, nil
}

func asNumberMap(v any) (*map[string]Number, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("want an object, got %s", kind(v))
	}
	out := make(map[string]Number, len(obj))
	for k, x := range obj {
		n, err := asNumber(x)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = *n
	}
	return &out, nil
}

func nullable[T any](v any, as func(any) (*T, error)) (Nullable[T], error) {
	if v == nil {
		return Nullable[T]{Set: true, Null: true}, nil
	}
	p, err := as(v)
	if err != nil {
		return Nullable[T]{}, err
	}
	return Nullable[T]{Set: true, V: *p}, nil
}

func (n Nullable[T]) value() any {
	if n.Null {
		return nil
	}
	return n.V
}

func stringsValue(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

func numberMapValue(m map[string]Number) map[string]any {
	out := make(map[string]any, len(m))
	for k, n := range m {
		out[k] = n
	}
	return out
}

func kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case json.Number:
		return "a number"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}
