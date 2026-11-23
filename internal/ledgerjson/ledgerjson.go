// Package ledgerjson is the one encoder for ledger lines. Its bytes equal what the Python
// ledger writes with json.dumps(record, separators=(",", ":"), sort_keys=True): sorted keys,
// no spaces, every non-ASCII character as a \uXXXX escape, and Python's float repr.
package ledgerjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Marshal encodes v. It accepts nil, bool, string, int, int64, float64, json.Number,
// []any and map[string]any, nested in any combination.
func Marshal(v any) ([]byte, error) {
	var b strings.Builder
	if err := encode(&b, v, true); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// MarshalUTF8 is json.dumps(v, ensure_ascii=False, separators=(",", ":")): the same, but
// non-ASCII characters stay as they are and only quotes, backslashes and control characters
// are escaped. It is for measuring a value's size as Python would, not for ledger lines.
func MarshalUTF8(v any) ([]byte, error) {
	var b strings.Builder
	if err := encode(&b, v, false); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func encode(b *strings.Builder, v any, ascii bool) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		writeString(b, x, ascii)
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		b.WriteString(FormatFloat(x))
	case json.Number:
		return writeNumber(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := encode(b, e, ascii); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// Byte order of UTF-8 is code point order, which is how Python sorts str keys.
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k, ascii)
			b.WriteByte(':')
			if err := encode(b, x[k], ascii); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("ledgerjson: unsupported type %T", v)
	}
	return nil
}

// writeNumber keeps an integer literal's digits (Python ints have no size limit) and
// re-formats any other number as the float Python would hold.
func writeNumber(b *strings.Builder, n json.Number) error {
	s := string(n)
	if !strings.ContainsAny(s, ".eE") {
		if !isInteger(s) {
			return fmt.Errorf("ledgerjson: invalid integer %q", s)
		}
		b.WriteString(s)
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	// Python reads 1e999 as infinity, which ParseFloat returns with a range error.
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return fmt.Errorf("ledgerjson: invalid number %q: %w", s, err)
	}
	b.WriteString(FormatFloat(f))
	return nil
}

func isInteger(s string) bool {
	d := strings.TrimPrefix(s, "-")
	if d == "" || (len(d) > 1 && d[0] == '0') {
		return false
	}
	for _, c := range d {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// FormatFloat is Python's repr(float): the shortest digits that round-trip, written in
// fixed notation when the decimal point falls within 16 places, else as d.ddde±XX.
func FormatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	sign := ""
	if math.Signbit(f) {
		sign = "-"
	}
	sci := strconv.FormatFloat(math.Abs(f), 'e', -1, 64) // d[.ddd]e±XX
	mant, expPart, _ := strings.Cut(sci, "e")
	exp, _ := strconv.Atoi(expPart)
	digits := strings.Replace(mant, ".", "", 1)
	decpt := exp + 1 // value = 0.<digits> × 10^decpt
	if decpt <= -4 || decpt > 16 {
		out := digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		e := decpt - 1
		esign := "+"
		if e < 0 {
			esign, e = "-", -e
		}
		return fmt.Sprintf("%s%se%s%02d", sign, out, esign, e)
	}
	switch {
	case decpt <= 0:
		return sign + "0." + strings.Repeat("0", -decpt) + digits
	case decpt < len(digits):
		return sign + digits[:decpt] + "." + digits[decpt:]
	default:
		return sign + digits + strings.Repeat("0", decpt-len(digits)) + ".0"
	}
}

const hexDigits = "0123456789abcdef"

func writeString(b *strings.Builder, s string, ascii bool) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				writeU(b, r)
			case r < 0x7f: // Python escapes DEL too
				b.WriteRune(r)
			case !ascii:
				b.WriteRune(r)
			case r > 0xFFFF:
				hi, lo := utf16.EncodeRune(r)
				writeU(b, hi)
				writeU(b, lo)
			default:
				writeU(b, r)
			}
		}
	}
	b.WriteByte('"')
}

func writeU(b *strings.Builder, r rune) {
	b.WriteString(`\u`)
	for shift := 12; shift >= 0; shift -= 4 {
		b.WriteByte(hexDigits[(r>>shift)&0xF])
	}
}
