package relay

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/0xbew/livy/internal/otelredact"
)

// OTLP/HTTP's default encoding is protobuf, and an exporter whose protocol Livy cannot set
// (Claude Code under managed settings) sends it. protoExport turns a protobuf logs or traces
// export into the OTLP/JSON document the redactor and the stream already read, as Claude
// Code's JSON exporter writes it: lowerCamelCase names, ids as hex, times as decimal strings,
// enums and intValue as numbers. A field this schema does not list is skipped unread, as the
// redactor drops one; a malformed message refuses the export.

type protoKind int

const (
	pString  protoKind = iota + 1
	pHex               // bytes as lowercase hex: trace and span ids
	pBase64            // bytes as base64: bytesValue
	pUint32            // varint, a JSON number
	pInt64             // varint, two's complement, a JSON number
	pBool              // varint
	pFixed64           // a decimal string: times in nanoseconds
	pFixed32           // a JSON number: flags
	pDouble            // a JSON number
	pMessage
)

type protoField struct {
	name     string
	kind     protoKind
	msg      string // the message kind, for pMessage
	repeated bool
}

func msgField(name, msg string, repeated bool) protoField {
	return protoField{name: name, kind: pMessage, msg: msg, repeated: repeated}
}

// protoSchema is opentelemetry-proto's v1 logs and trace messages, by field number.
var protoSchema = map[string]map[uint64]protoField{
	otelredact.Logs:   {1: msgField("resourceLogs", "resourceLogs", true)},
	otelredact.Traces: {1: msgField("resourceSpans", "resourceSpans", true)},
	"resourceLogs": {1: msgField("resource", "resource", false), 2: msgField("scopeLogs", "scopeLogs", true),
		3: {name: "schemaUrl", kind: pString}},
	"resourceSpans": {1: msgField("resource", "resource", false), 2: msgField("scopeSpans", "scopeSpans", true),
		3: {name: "schemaUrl", kind: pString}},
	"resource": {1: msgField("attributes", "keyValue", true), 2: {name: "droppedAttributesCount", kind: pUint32}},
	"scopeLogs": {1: msgField("scope", "scope", false), 2: msgField("logRecords", "logRecord", true),
		3: {name: "schemaUrl", kind: pString}},
	"scopeSpans": {1: msgField("scope", "scope", false), 2: msgField("spans", "span", true),
		3: {name: "schemaUrl", kind: pString}},
	"scope": {1: {name: "name", kind: pString}, 2: {name: "version", kind: pString},
		3: msgField("attributes", "keyValue", true), 4: {name: "droppedAttributesCount", kind: pUint32}},
	"keyValue": {1: {name: "key", kind: pString}, 2: msgField("value", "anyValue", false)},
	"anyValue": {1: {name: "stringValue", kind: pString}, 2: {name: "boolValue", kind: pBool},
		3: {name: "intValue", kind: pInt64}, 4: {name: "doubleValue", kind: pDouble},
		5: msgField("arrayValue", "arrayValue", false), 6: msgField("kvlistValue", "kvlistValue", false),
		7: {name: "bytesValue", kind: pBase64}},
	"arrayValue":  {1: msgField("values", "anyValue", true)},
	"kvlistValue": {1: msgField("values", "keyValue", true)},
	"logRecord": {1: {name: "timeUnixNano", kind: pFixed64}, 11: {name: "observedTimeUnixNano", kind: pFixed64},
		2: {name: "severityNumber", kind: pUint32}, 3: {name: "severityText", kind: pString},
		5: msgField("body", "anyValue", false), 6: msgField("attributes", "keyValue", true),
		7: {name: "droppedAttributesCount", kind: pUint32}, 8: {name: "flags", kind: pFixed32},
		9: {name: "traceId", kind: pHex}, 10: {name: "spanId", kind: pHex}, 12: {name: "eventName", kind: pString}},
	"span": {1: {name: "traceId", kind: pHex}, 2: {name: "spanId", kind: pHex}, 3: {name: "traceState", kind: pString},
		4: {name: "parentSpanId", kind: pHex}, 16: {name: "flags", kind: pFixed32}, 5: {name: "name", kind: pString},
		6: {name: "kind", kind: pUint32}, 7: {name: "startTimeUnixNano", kind: pFixed64},
		8: {name: "endTimeUnixNano", kind: pFixed64}, 9: msgField("attributes", "keyValue", true),
		10: {name: "droppedAttributesCount", kind: pUint32}, 11: msgField("events", "event", true),
		12: {name: "droppedEventsCount", kind: pUint32}, 13: msgField("links", "link", true),
		14: {name: "droppedLinksCount", kind: pUint32}, 15: msgField("status", "status", false)},
	"event": {1: {name: "timeUnixNano", kind: pFixed64}, 2: {name: "name", kind: pString},
		3: msgField("attributes", "keyValue", true), 4: {name: "droppedAttributesCount", kind: pUint32}},
	"link": {1: {name: "traceId", kind: pHex}, 2: {name: "spanId", kind: pHex}, 3: {name: "traceState", kind: pString},
		4: msgField("attributes", "keyValue", true), 5: {name: "droppedAttributesCount", kind: pUint32},
		6: {name: "flags", kind: pFixed32}},
	"status": {2: {name: "message", kind: pString}, 3: {name: "code", kind: pUint32}},
}

// maxProtoDepth bounds nesting: an AnyValue can hold lists of AnyValues without end.
const maxProtoDepth = 64

var errProto = errors.New("not an OTLP protobuf export")

// protoExport decodes a logs or traces export; another signal is an error.
func protoExport(signal string, b []byte) (map[string]any, error) {
	if signal != otelredact.Logs && signal != otelredact.Traces {
		return nil, fmt.Errorf("protobuf %s exports are not decoded; send them as OTLP/JSON", signal)
	}
	return protoMessage(signal, b, 0)
}

func protoMessage(kind string, b []byte, depth int) (map[string]any, error) {
	if depth > maxProtoDepth {
		return nil, fmt.Errorf("%w: nested deeper than %d", errProto, maxProtoDepth)
	}
	fields := protoSchema[kind]
	m := map[string]any{}
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, errProto
		}
		b = b[n:]
		num, wire := tag>>3, tag&7
		raw, rest, err := protoValue(wire, b)
		if err != nil {
			return nil, err
		}
		b = rest
		f, known := fields[num]
		if !known {
			continue
		}
		v, err := protoDecode(f, wire, raw, depth)
		if err != nil {
			return nil, err
		}
		if f.repeated {
			l, _ := m[f.name].([]any)
			m[f.name] = append(l, v)
			continue
		}
		m[f.name] = v
	}
	return m, nil
}

// protoValue splits one field's value off b by its wire type: a varint's or a fixed width's
// bytes as they are, a length-delimited field's payload.
func protoValue(wire uint64, b []byte) (raw, rest []byte, err error) {
	switch wire {
	case 0:
		_, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, nil, errProto
		}
		return b[:n], b[n:], nil
	case 1, 5:
		w := 8
		if wire == 5 {
			w = 4
		}
		if len(b) < w {
			return nil, nil, errProto
		}
		return b[:w], b[w:], nil
	case 2:
		l, n := binary.Uvarint(b)
		if n <= 0 || l > uint64(len(b)-n) {
			return nil, nil, errProto
		}
		end := n + int(l)
		return b[n:end], b[end:], nil
	}
	return nil, nil, fmt.Errorf("%w: wire type %d", errProto, wire)
}

// protoWire is the wire type each kind is sent with.
var protoWire = map[protoKind]uint64{pString: 2, pHex: 2, pBase64: 2, pMessage: 2, pUint32: 0, pInt64: 0, pBool: 0,
	pFixed64: 1, pDouble: 1, pFixed32: 5}

func protoDecode(f protoField, wire uint64, raw []byte, depth int) (any, error) {
	if protoWire[f.kind] != wire {
		return nil, fmt.Errorf("%w: %s sent with wire type %d", errProto, f.name, wire)
	}
	switch f.kind {
	case pString:
		return string(raw), nil
	case pHex:
		return hex.EncodeToString(raw), nil
	case pBase64:
		return base64.StdEncoding.EncodeToString(raw), nil
	case pMessage:
		return protoMessage(f.msg, raw, depth+1)
	case pFixed64:
		return strconv.FormatUint(binary.LittleEndian.Uint64(raw), 10), nil
	case pFixed32:
		return json.Number(strconv.FormatUint(uint64(binary.LittleEndian.Uint32(raw)), 10)), nil
	case pDouble:
		return protoDouble(math.Float64frombits(binary.LittleEndian.Uint64(raw))), nil
	}
	u, _ := binary.Uvarint(raw)
	switch f.kind {
	case pInt64:
		return json.Number(strconv.FormatInt(int64(u), 10)), nil
	case pBool:
		return u != 0, nil
	}
	return json.Number(strconv.FormatUint(u, 10)), nil
}

// protoDouble is a double as OTLP/JSON writes it: a number, or a string for the values JSON
// has no number for.
func protoDouble(f float64) any {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return json.Number(strconv.FormatFloat(f, 'g', -1, 64))
}
