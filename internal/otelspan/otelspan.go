// Package otelspan reads the trace spans in Livy's otel stream: one OTLP/JSON export per line, as
// the receiver stores it. It keeps each span's ids, name, times and its string and boolean
// attributes (a boolean as "true" or "false"), which is all `livy profile` reads.
package otelspan

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// MaxLine is the longest otel stream line read: one export can hold many spans.
const MaxLine = 64 << 20

// tracesSignal is the stream's signal name for a line of spans.
const tracesSignal = "traces"

// Span is one stored OTEL span.
type Span struct {
	Trace, ID, Parent string
	Name              string
	Start, End        time.Time
	Attrs             map[string]string
}

// otlpLine is one line of the otel stream: one export as the receiver stored it.
type otlpLine struct {
	Signal string `json:"signal"`
	OTLP   struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []otlpSpan `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	} `json:"otlp"`
}

type otlpSpan struct {
	TraceID      string     `json:"traceId"`
	SpanID       string     `json:"spanId"`
	ParentSpanID string     `json:"parentSpanId"`
	Name         string     `json:"name"`
	Start        nanos      `json:"startTimeUnixNano"`
	End          nanos      `json:"endTimeUnixNano"`
	Attributes   []otlpAttr `json:"attributes"`
}

type otlpAttr struct {
	Key   string `json:"key"`
	Value struct {
		String *string `json:"stringValue"`
		Bool   *bool   `json:"boolValue"`
	} `json:"value"`
}

// nanos is an OTLP/JSON time: a decimal string of nanoseconds since the epoch (or a number).
type nanos int64

func (n *nanos) UnmarshalJSON(b []byte) error {
	v, err := strconv.ParseInt(strings.Trim(string(b), `"`), 10, 64)
	if err != nil {
		return fmt.Errorf("unix nanoseconds %s: %w", b, err)
	}
	*n = nanos(v)
	return nil
}

// Decode is the spans in one otel stream line; none for a line of another signal. An error
// means the line could not be read.
func Decode(line []byte) ([]Span, error) {
	var l otlpLine
	if err := json.Unmarshal(line, &l); err != nil {
		return nil, fmt.Errorf("otel stream line: %w", err)
	}
	if l.Signal != tracesSignal {
		return nil, nil
	}
	var spans []Span
	for _, rs := range l.OTLP.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				spans = append(spans, s.span())
			}
		}
	}
	return spans, nil
}

// Read reads the trace spans in r, one otel stream line each; other signals' lines are
// skipped. bad counts the lines that could not be read.
func Read(r io.Reader) (spans []Span, bad int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), MaxLine)
	for sc.Scan() {
		got, err := Decode(sc.Bytes())
		if err != nil {
			bad++
			continue
		}
		spans = append(spans, got...)
	}
	if err := sc.Err(); err != nil {
		return spans, bad, fmt.Errorf("read otel stream: %w", err)
	}
	return spans, bad, nil
}

func (s otlpSpan) span() Span {
	attrs := make(map[string]string, len(s.Attributes))
	for _, a := range s.Attributes {
		switch {
		case a.Value.String != nil:
			attrs[a.Key] = *a.Value.String
		case a.Value.Bool != nil:
			attrs[a.Key] = strconv.FormatBool(*a.Value.Bool)
		}
	}
	return Span{Trace: s.TraceID, ID: s.SpanID, Parent: s.ParentSpanID, Name: s.Name,
		Start: time.Unix(0, int64(s.Start)).UTC(), End: time.Unix(0, int64(s.End)).UTC(), Attrs: attrs}
}
