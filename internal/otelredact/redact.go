// Package otelredact removes identity and content from an OTLP/JSON export, by the same
// rules as the collector (deploy/otel/collector.yaml), so Livy's own receiver stores what a
// collector would. It works on the decoded JSON of an ExportLogsServiceRequest,
// ExportMetricsServiceRequest or ExportTraceServiceRequest. A shape it cannot walk is an
// error, so the caller refuses the export rather than storing it unredacted.
package otelredact

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Signals, by the OTLP/HTTP path they arrive on.
const (
	Logs    = "logs"
	Metrics = "metrics"
	Traces  = "traces"
)

var errShape = errors.New("not an OTLP/JSON export")

// fields are the OTLP/JSON field names kept on each message: everything else (a field a
// client adds later, the proto field names in snake_case) is dropped unread, so no text can
// reach storage under a name this walker does not visit.
var fields = map[string]map[string]bool{
	"resourceLogs":    keySet("resource", "scopeLogs", "schemaUrl"),
	"resourceMetrics": keySet("resource", "scopeMetrics", "schemaUrl"),
	"resourceSpans":   keySet("resource", "scopeSpans", "schemaUrl"),
	"resource":        keySet("attributes", "droppedAttributesCount"),
	"scopeLogs":       keySet("scope", "logRecords", "schemaUrl"),
	"scopeMetrics":    keySet("scope", "metrics", "schemaUrl"),
	"scopeSpans":      keySet("scope", "spans", "schemaUrl"),
	"scope":           keySet("name", "version", "attributes", "droppedAttributesCount"),
	"logRecords": keySet("timeUnixNano", "observedTimeUnixNano", "severityNumber", "severityText", "body",
		"attributes", "droppedAttributesCount", "flags", "traceId", "spanId", "eventName"),
	"metrics": keySet("name", "description", "unit", "gauge", "sum", "histogram", "exponentialHistogram",
		"summary", "metadata"),
	"gauge":                keySet("dataPoints"),
	"sum":                  keySet("dataPoints", "aggregationTemporality", "isMonotonic"),
	"histogram":            keySet("dataPoints", "aggregationTemporality"),
	"exponentialHistogram": keySet("dataPoints", "aggregationTemporality"),
	"summary":              keySet("dataPoints"),
	"dataPoints": keySet("attributes", "startTimeUnixNano", "timeUnixNano", "asDouble", "asInt", "exemplars",
		"flags", "count", "sum", "bucketCounts", "explicitBounds", "min", "max", "scale", "zeroCount", "positive",
		"negative", "zeroThreshold", "quantileValues"),
	"exemplars": keySet("filteredAttributes", "timeUnixNano", "asDouble", "asInt", "spanId", "traceId"),
	"spans": keySet("traceId", "spanId", "traceState", "parentSpanId", "flags", "name", "kind",
		"startTimeUnixNano", "endTimeUnixNano", "attributes", "droppedAttributesCount", "events",
		"droppedEventsCount", "links", "droppedLinksCount", "status"),
	"events": keySet("timeUnixNano", "name", "attributes", "droppedAttributesCount"),
	"links":  keySet("traceId", "spanId", "traceState", "attributes", "droppedAttributesCount", "flags"),
	"status": keySet("message", "code"),
	// An attribute value, and the list inside an arrayValue or kvlistValue.
	"anyValue":  keySet("stringValue", "boolValue", "intValue", "doubleValue", "arrayValue", "kvlistValue", "bytesValue"),
	"valueList": keySet("values"),
}

func prune(m map[string]any, kind string) {
	keep := fields[kind]
	for k := range m {
		if !keep[k] {
			delete(m, k)
		}
	}
}

// Redact edits doc in place. It returns the number of resource entries (0: nothing to keep).
func Redact(signal string, doc map[string]any) (int, error) {
	top := map[string]string{Logs: "resourceLogs", Metrics: "resourceMetrics", Traces: "resourceSpans"}[signal]
	if top == "" {
		return 0, fmt.Errorf("unknown signal %q", signal)
	}
	for k := range doc {
		if k != top {
			delete(doc, k) // nothing else belongs in an export request
		}
	}
	resources, err := list(doc[top], top)
	if err != nil {
		return 0, err
	}
	for _, r := range resources {
		res, ok := r.(map[string]any)
		if !ok {
			return 0, errShape
		}
		if err := redactResource(signal, top, res); err != nil {
			return 0, err
		}
	}
	// Last, every kept field must hold its own type: an object where a string or a timestamp
	// belongs would carry text no rule above looked at.
	if err := check(doc, signal); err != nil {
		return 0, err
	}
	return len(resources), nil
}

// redactResource redacts one resource of a signal's export: its own attributes down to
// identity, then each record by the signal's rules.
func redactResource(signal, top string, res map[string]any) error {
	prune(res, top)
	claude := false
	if v, has := res["resource"]; has {
		rs, ok := v.(map[string]any)
		if !ok {
			return errShape
		}
		prune(rs, "resource")
		claude = stringAttr(rs["attributes"]) == claudeService
		if err := attrs(rs, "attributes", identity); err != nil {
			return err
		}
	}
	switch signal {
	case Logs:
		return eachScope(res, "scopeLogs", "logRecords", func(rec map[string]any) error {
			return redactLog(rec, claude)
		})
	case Metrics:
		return eachScope(res, "scopeMetrics", "metrics", func(m map[string]any) error {
			return redactMetric(m, claude)
		})
	case Traces:
		return eachScope(res, "scopeSpans", "spans", func(s map[string]any) error {
			return redactSpan(s, claude)
		})
	}
	return nil
}

// shape is each message's fields and what each holds: a message kind, "list:<kind>",
// "attrs" (key-value list), "any" (an AnyValue), "scalar", or "scalars" (a list of scalars).
var shape = map[string]map[string]string{
	Logs:              {"resourceLogs": "list:resourceLogs"},
	Metrics:           {"resourceMetrics": "list:resourceMetrics"},
	Traces:            {"resourceSpans": "list:resourceSpans"},
	"resourceLogs":    {"resource": "resource", "scopeLogs": "list:scopeLogs", "schemaUrl": "scalar"},
	"resourceMetrics": {"resource": "resource", "scopeMetrics": "list:scopeMetrics", "schemaUrl": "scalar"},
	"resourceSpans":   {"resource": "resource", "scopeSpans": "list:scopeSpans", "schemaUrl": "scalar"},
	"resource":        {"attributes": "attrs", "droppedAttributesCount": "scalar"},
	"scope":           {"name": "scalar", "version": "scalar", "attributes": "attrs", "droppedAttributesCount": "scalar"},
	"scopeLogs":       {"scope": "scope", "logRecords": "list:logRecord", "schemaUrl": "scalar"},
	"scopeMetrics":    {"scope": "scope", "metrics": "list:metric", "schemaUrl": "scalar"},
	"scopeSpans":      {"scope": "scope", "spans": "list:span", "schemaUrl": "scalar"},
	"logRecord": {"timeUnixNano": "scalar", "observedTimeUnixNano": "scalar", "severityNumber": "scalar",
		"severityText": "scalar", "body": "any", "attributes": "attrs", "droppedAttributesCount": "scalar",
		"flags": "scalar", "traceId": "scalar", "spanId": "scalar", "eventName": "scalar"},
	"metric": {"name": "scalar", "description": "scalar", "unit": "scalar", "gauge": "points", "sum": "points",
		"histogram": "points", "exponentialHistogram": "points", "summary": "points", "metadata": "attrs"},
	"points": {"dataPoints": "list:point", "aggregationTemporality": "scalar", "isMonotonic": "scalar"},
	"point": {"attributes": "attrs", "startTimeUnixNano": "scalar", "timeUnixNano": "scalar", "asDouble": "scalar",
		"asInt": "scalar", "exemplars": "list:exemplar", "flags": "scalar", "count": "scalar", "sum": "scalar",
		"bucketCounts": "scalars", "explicitBounds": "scalars", "min": "scalar", "max": "scalar", "scale": "scalar",
		"zeroCount": "scalar", "positive": "buckets", "negative": "buckets", "zeroThreshold": "scalar",
		"quantileValues": "list:quantile"},
	"buckets":  {"offset": "scalar", "bucketCounts": "scalars"},
	"quantile": {"quantile": "scalar", "value": "scalar"},
	"exemplar": {"filteredAttributes": "attrs", "timeUnixNano": "scalar", "asDouble": "scalar", "asInt": "scalar",
		"spanId": "scalar", "traceId": "scalar"},
	"span": {"traceId": "scalar", "spanId": "scalar", "traceState": "scalar", "parentSpanId": "scalar",
		"flags": "scalar", "name": "scalar", "kind": "scalar", "startTimeUnixNano": "scalar",
		"endTimeUnixNano": "scalar", "attributes": "attrs", "droppedAttributesCount": "scalar",
		"events": "list:event", "droppedEventsCount": "scalar", "links": "list:link", "droppedLinksCount": "scalar",
		"status": "status"},
	"event":  {"timeUnixNano": "scalar", "name": "scalar", "attributes": "attrs", "droppedAttributesCount": "scalar"},
	"link":   {"traceId": "scalar", "spanId": "scalar", "traceState": "scalar", "attributes": "attrs", "droppedAttributesCount": "scalar", "flags": "scalar"},
	"status": {"message": "scalar", "code": "scalar"},
}

// check walks v as kind t: a field the shape does not list is dropped, and a field holding
// the wrong type refuses the export.
func check(v any, t string) error {
	if v == nil {
		return nil
	}
	switch {
	case t == "scalar":
		return checkScalar(v)
	case t == "scalars":
		return checkList(v, "scalar")
	case t == "attrs":
		return checkAttrs(v)
	case t == "any":
		return checkAny(v)
	case strings.HasPrefix(t, "list:"):
		return checkList(v, strings.TrimPrefix(t, "list:"))
	}
	return checkMessage(v, t)
}

func checkScalar(v any) error {
	switch v.(type) {
	case string, bool, float64, json.Number:
		return nil
	}
	return errShape
}

// checkList walks a list whose every item is of kind t.
func checkList(v any, t string) error {
	l, ok := v.([]any)
	if !ok {
		return errShape
	}
	for _, x := range l {
		if err := check(x, t); err != nil {
			return err
		}
	}
	return nil
}

// checkAttrs walks a key-value list: each item a string key and an AnyValue.
func checkAttrs(v any) error {
	l, ok := v.([]any)
	if !ok {
		return errShape
	}
	for _, x := range l {
		kv, ok := x.(map[string]any)
		if !ok {
			return errShape
		}
		if _, ok := kv["key"].(string); !ok {
			return errShape
		}
		if err := check(kv["value"], "any"); err != nil {
			return err
		}
	}
	return nil
}

// checkAny walks an AnyValue, dropping a field it does not know.
func checkAny(v any) error {
	m, ok := v.(map[string]any)
	if !ok {
		return errShape
	}
	for k, x := range m {
		known, err := checkAnyField(k, x)
		if err != nil {
			return err
		}
		if !known {
			delete(m, k)
		}
	}
	return nil
}

// checkAnyField checks one AnyValue field; false when the field is not one AnyValue has.
func checkAnyField(k string, x any) (bool, error) {
	switch k {
	case "stringValue", "bytesValue":
		if _, ok := x.(string); !ok {
			return true, errShape
		}
	case "boolValue":
		if _, ok := x.(bool); !ok {
			return true, errShape
		}
	case "intValue", "doubleValue":
		if err := check(x, "scalar"); err != nil {
			return true, err
		}
		if _, isBool := x.(bool); isBool {
			return true, errShape
		}
	case "arrayValue", "kvlistValue":
		return true, checkValueList(k, x)
	default:
		return false, nil
	}
	return true, nil
}

// checkValueList walks the values of an arrayValue (AnyValues) or a kvlistValue (key-value
// pairs), dropping every other field.
func checkValueList(k string, x any) error {
	inner, ok := x.(map[string]any)
	if !ok {
		return errShape
	}
	for f := range inner {
		if f != "values" {
			delete(inner, f)
		}
	}
	want := "list:any"
	if k == "kvlistValue" {
		want = "attrs"
	}
	return check(inner["values"], want)
}

// checkMessage walks a message of kind t, dropping a field its shape does not list.
func checkMessage(v any, t string) error {
	fieldsOf, known := shape[t]
	m, ok := v.(map[string]any)
	if !known || !ok {
		return errShape
	}
	for k, x := range m {
		want, listed := fieldsOf[k]
		if !listed {
			delete(m, k)
			continue
		}
		if err := check(x, want); err != nil {
			return err
		}
	}
	return nil
}

func list(v any, what string) ([]any, error) {
	if v == nil {
		return nil, nil
	}
	l, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is not a list", what)
	}
	return l, nil
}

// eachScope walks res[scopes][i] (redacting the scope's own attributes) and calls fn on each
// item of its items list.
func eachScope(res map[string]any, scopes, items string, fn func(map[string]any) error) error {
	return eachObject(res[scopes], scopes, func(sm map[string]any) error {
		if v, has := sm["scope"]; has {
			sc, ok := v.(map[string]any)
			if !ok {
				return errShape
			}
			prune(sc, "scope")
			if err := attrs(sc, "attributes", identity); err != nil {
				return err
			}
		}
		return eachObject(sm[items], items, fn)
	})
}

func identity(k string) bool { return !identityPrefix.MatchString(k) && !identityKeys[k] }

func redactLog(rec map[string]any, claude bool) error {
	keep := func(k string) bool { return identity(k) && !logContent[k] }
	if !claude {
		keep = func(k string) bool { return identity(k) && !logContent[k] && foreignLogKeep[k] }
		if _, has := rec["body"]; has {
			rec["body"] = map[string]any{"stringValue": ""}
		}
	}
	if err := nested(rec["body"], keep); err != nil {
		return err
	}
	return attrs(rec, "attributes", keep)
}

// eachObject calls fn on each item of the list v, pruned to kind's fields; an item that is
// not an object refuses the export.
func eachObject(v any, kind string, fn func(map[string]any) error) error {
	items, err := list(v, kind)
	if err != nil {
		return err
	}
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return errShape
		}
		prune(m, kind)
		if err := fn(m); err != nil {
			return err
		}
	}
	return nil
}

func redactMetric(m map[string]any, claude bool) error {
	keep := identity
	if !claude {
		keep = func(k string) bool { return identity(k) && foreignPointKeep[k] }
	}
	if err := attrs(m, "metadata", keep); err != nil {
		return err
	}
	for _, kind := range []string{"gauge", "sum", "histogram", "exponentialHistogram", "summary"} {
		v, has := m[kind]
		if !has {
			continue
		}
		body, ok := v.(map[string]any)
		if !ok {
			return errShape
		}
		prune(body, kind)
		err := eachObject(body["dataPoints"], "dataPoints", func(pm map[string]any) error {
			return redactPoint(pm, keep)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func redactPoint(pm map[string]any, keep func(string) bool) error {
	if err := attrs(pm, "attributes", keep); err != nil {
		return err
	}
	return eachObject(pm["exemplars"], "exemplars", func(em map[string]any) error {
		return attrs(em, "filteredAttributes", keep)
	})
}

func redactSpan(s map[string]any, claude bool) error {
	keep := func(k string) bool { return identity(k) && !spanContent[k] }
	if !claude {
		keep = func(k string) bool { return identity(k) && !spanContent[k] && foreignSpanKeep[k] }
	}
	if err := attrs(s, "attributes", keep); err != nil {
		return err
	}
	if v, has := s["status"]; has {
		st, ok := v.(map[string]any)
		if !ok {
			return errShape
		}
		prune(st, "status")
		if _, has := st["message"]; has {
			st["message"] = ""
		}
	}
	// Link attributes, which the collector cannot reach: Livy holds them to the span's rules.
	err := eachObject(s["links"], "links", func(lm map[string]any) error {
		return attrs(lm, "attributes", keep)
	})
	if err != nil {
		return err
	}
	return redactEvents(s, claude)
}

// redactEvents keeps a Claude span's one kept event, and every other client's events with
// their names held to a plain name.
func redactEvents(s map[string]any, claude bool) error {
	events, err := list(s["events"], "events")
	if err != nil {
		return err
	}
	var kept []any
	for _, e := range events {
		em, ok := e.(map[string]any)
		if !ok {
			return errShape
		}
		prune(em, "events")
		name, _ := em["name"].(string)
		if claude && name != claudeEventKeep {
			continue
		}
		ekeep := identity
		if !claude {
			if !eventName.MatchString(name) {
				em["name"] = "event"
			}
			ekeep = func(k string) bool { return identity(k) && foreignEventKeep[k] }
		}
		if err := attrs(em, "attributes", ekeep); err != nil {
			return err
		}
		kept = append(kept, em)
	}
	if _, has := s["events"]; has {
		if kept == nil {
			kept = []any{}
		}
		s["events"] = kept
	}
	return nil
}

// attrs keeps only the key-value entries of o[field] whose key passes keep.
func attrs(o map[string]any, field string, keep func(string) bool) error {
	l, err := list(o[field], field)
	if err != nil || l == nil {
		return err
	}
	out := make([]any, 0, len(l))
	for _, kv := range l {
		m, ok := kv.(map[string]any)
		if !ok {
			return errShape
		}
		k, ok := m["key"].(string)
		if !ok {
			return fmt.Errorf("an attribute in %s has no key", field)
		}
		if !keep(k) {
			continue
		}
		if err := keepAttr(m, keep); err != nil {
			return err
		}
		out = append(out, m)
	}
	o[field] = out
	return nil
}

// keepAttr strips a kept attribute to its key and value, and redacts inside the value.
func keepAttr(m map[string]any, keep func(string) bool) error {
	for f := range m {
		if f != "key" && f != "value" {
			delete(m, f)
		}
	}
	return nested(m["value"], keep)
}

// nested holds an AnyValue to its own fields, and holds the keys inside its key-value lists,
// at any depth, to the rules of the attribute it sits in: a value must not carry user.email,
// or a key the attribute's allowlist leaves out, one level down, nor text under a field
// AnyValue does not have.
func nested(v any, keep func(string) bool) error {
	if v == nil {
		return nil
	}
	val, ok := v.(map[string]any)
	if !ok {
		return errShape
	}
	prune(val, "anyValue")
	for _, kind := range []string{"kvlistValue", "arrayValue"} {
		inner, has := val[kind]
		if !has {
			continue
		}
		im, ok := inner.(map[string]any)
		if !ok {
			return errShape
		}
		prune(im, "valueList")
		if kind == "kvlistValue" {
			// A list inside an allowed attribute is held to that attribute's own rules.
			if err := attrs(im, "values", keep); err != nil {
				return err
			}
			continue
		}
		items, err := list(im["values"], "values")
		if err != nil {
			return err
		}
		for _, it := range items {
			if err := nested(it, keep); err != nil {
				return err
			}
		}
	}
	return nil
}

// stringAttr is the resource's service.name, "" when absent.
func stringAttr(v any) string {
	l, _ := v.([]any)
	for _, kv := range l {
		m, _ := kv.(map[string]any)
		if k, _ := m["key"].(string); k == "service.name" {
			val, _ := m["value"].(map[string]any)
			s, _ := val["stringValue"].(string)
			return s
		}
	}
	return ""
}
