// Package pytext reproduces the few Python behaviours the ledger's bytes depend on: what
// str.strip() removes, repr() of a str, bytes.decode("utf-8", "replace"), and bool() of a
// decoded JSON value.
package pytext

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// IsSpace is str.isspace() for one character: Go's unicode.IsSpace plus U+001C..U+001F,
// which Python counts as whitespace.
func IsSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// IsWord is a \w character in a Python str pattern (alphanumeric or underscore).
func IsWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

// Strip is str.strip() with no argument.
func Strip(s string) string { return strings.TrimFunc(s, IsSpace) }

// Repr is repr() of a str.
func Repr(s string) string {
	q := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(q):
			b.WriteByte('\\')
			b.WriteByte(q)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(q)
	return b.String()
}

// DecodeReplace is bytes.decode("utf-8", "replace"): each maximal invalid subpart becomes
// one U+FFFD, so a truncated sequence is one replacement, a stray byte one each.
func DecodeReplace(p []byte) string {
	if utf8.Valid(p) {
		return string(p)
	}
	var b strings.Builder
	for i := 0; i < len(p); {
		r, n := utf8.DecodeRune(p[i:])
		if r != utf8.RuneError || n > 1 {
			b.WriteRune(r)
			i += n
			continue
		}
		b.WriteRune(utf8.RuneError)
		i += validPrefix(p[i:])
	}
	return b.String()
}

// validPrefix is how many bytes of an invalid sequence form the start of a valid one.
func validPrefix(p []byte) int {
	c := p[0]
	var need int
	lo, hi := byte(0x80), byte(0xbf)
	switch {
	case c >= 0xc2 && c <= 0xdf:
		need = 1
	case c == 0xe0:
		need, lo = 2, 0xa0
	case c == 0xed:
		need, hi = 2, 0x9f
	case c >= 0xe1 && c <= 0xef:
		need = 2
	case c == 0xf0:
		need, lo = 3, 0x90
	case c == 0xf4:
		need, hi = 3, 0x8f
	case c >= 0xf1 && c <= 0xf3:
		need = 3
	default:
		return 1
	}
	n := 1
	for k := 0; k < need && n < len(p); k++ {
		if p[n] < lo || p[n] > hi {
			break
		}
		lo, hi = 0x80, 0xbf
		n++
	}
	return n
}

// TypeName is Python's type(x).__name__ for a value decoded from JSON.
func TypeName(v any) string {
	switch x := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	case interface{ String() string }:
		if strings.ContainsAny(x.String(), ".eE") {
			return "float"
		}
		return "int"
	}
	return fmt.Sprintf("%T", v)
}

// Truthy is Python's bool() of a decoded JSON value, decoded with or without UseNumber.
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case interface{ String() string }: // json.Number: zero in any spelling (0, -0.0, 0e5) is false
		s := strings.TrimLeft(x.String(), "-")
		return strings.Trim(strings.SplitN(strings.ToLower(s), "e", 2)[0], "0.") != ""
	}
	return true
}
