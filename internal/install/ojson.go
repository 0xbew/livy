package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Settings files belong to the engineer and to Claude Code. They are read and written in
// key order, with numbers kept as written, so a change of ours moves nothing else.

// object is a JSON object that keeps its key order.
type object struct {
	keys []string
	m    map[string]any
}

func newObject() *object { return &object{m: map[string]any{}} }

func (o *object) get(k string) (any, bool) { v, ok := o.m[k]; return v, ok }

func (o *object) set(k string, v any) {
	if _, ok := o.m[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.m[k] = v
}

func (o *object) del(k string) {
	if _, ok := o.m[k]; !ok {
		return
	}
	delete(o.m, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// parseObject reads a JSON document whose top level is an object.
func parseObject(raw []byte) (*object, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the top-level object")
	}
	o, ok := v.(*object)
	if !ok {
		return nil, errors.New("the top level is not a JSON object")
	}
	return o, nil
}

func parseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			o := newObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				o.set(k, v)
			}
			_, err := dec.Token()
			return o, err
		case '[':
			list := []any{}
			for dec.More() {
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			_, err := dec.Token()
			return list, err
		}
		return nil, fmt.Errorf("unexpected %v", t)
	default:
		return t, nil
	}
}

// encode writes v with two-space indentation and a final newline, keys in their order,
// without HTML escaping (a command with && stays readable).
func encode(v any) []byte {
	var b bytes.Buffer
	write(&b, v, "")
	b.WriteByte('\n')
	return b.Bytes()
}

func write(b *bytes.Buffer, v any, indent string) {
	in := indent + "  "
	switch x := v.(type) {
	case *object:
		if len(x.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range x.keys {
			b.WriteString(in)
			writeString(b, k)
			b.WriteString(": ")
			write(b, x.m[k], in)
			if i < len(x.keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "}")
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, e := range x {
			b.WriteString(in)
			write(b, e, in)
			if i < len(x)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "]")
	case string:
		writeString(b, x)
	case json.Number:
		b.WriteString(x.String())
	case bool:
		fmt.Fprint(b, x)
	case nil:
		b.WriteString("null")
	case int:
		fmt.Fprint(b, x)
	default:
		raw, _ := json.Marshal(x)
		b.Write(raw)
	}
}

func writeString(b *bytes.Buffer, s string) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	b.WriteString(strings.TrimSuffix(buf.String(), "\n"))
}

// clone deep-copies a parsed value, so a dry run can change a copy.
func clone(v any) any {
	switch x := v.(type) {
	case *object:
		o := newObject()
		for _, k := range x.keys {
			o.set(k, clone(x.m[k]))
		}
		return o
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = clone(e)
		}
		return out
	}
	return v
}

// same reports whether two parsed values are equal.
func same(a, b any) bool { return bytes.Equal(encode(a), encode(b)) }

// encodeLike encodes v in the style of raw, the file it was read from. A file with no byte
// outside ASCII (Python's json.dump escapes everything else as \uXXXX) is written back the
// same way, so a rewrite changes no byte outside what was edited.
func encodeLike(raw []byte, v any) []byte {
	out := encode(v)
	for _, c := range raw {
		if c >= 0x7f {
			return out
		}
	}
	var b bytes.Buffer
	for _, r := range string(out) {
		switch {
		case r < 0x7f:
			b.WriteRune(r)
		case r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.Bytes()
}
