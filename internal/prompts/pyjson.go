package prompts

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/0xbew/livy/internal/ledgerjson"
)

// dumps is json.dumps(v, sort_keys=True, ensure_ascii=asciiOnly) with Python's default
// separators, the form the prompt queue and the pass's state and counts are kept in.
func dumps(v any, asciiOnly bool) string {
	var b strings.Builder
	write(&b, v, asciiOnly)
	return b.String()
}

func write(b *strings.Builder, v any, asciiOnly bool) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		enc := ledgerjson.MarshalUTF8
		if asciiOnly {
			enc = ledgerjson.Marshal
		}
		raw, _ := enc(x)
		b.Write(raw)
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		b.WriteString(ledgerjson.FormatFloat(x))
	case json.Number:
		b.WriteString(string(x))
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			write(b, e, asciiOnly)
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			write(b, k, asciiOnly)
			b.WriteString(": ")
			write(b, x[k], asciiOnly)
		}
		b.WriteByte('}')
	default:
		panic(fmt.Sprintf("prompts: cannot encode %T", v))
	}
}
