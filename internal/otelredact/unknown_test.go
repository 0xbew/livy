package otelredact

import (
	"encoding/json"
	"strings"
	"testing"
)

// Guards: identity stored unredacted because it sits where the walker does not look: under a
// field name it does not know (the proto names in snake_case, a field a client adds later), on
// a span link, in a metric's metadata, or one level down inside a key-value list value.
func TestIdentityOutsideTheKnownFieldsIsNotStored(t *testing.T) {
	leak := `[{"key":"user.email","value":{"stringValue":"LEAK"}}]`
	for signal, doc := range map[string]string{
		Logs: `{"resourceLogs":[{"scope_logs":[{"log_records":[{"attributes":` + leak + `}]}]},` +
			`{"scopeLogs":[{"logRecords":[{"extra":"LEAK","body":{"kvlistValue":{"values":` + leak + `}}}]}]}]}`,
		Metrics: `{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"metadata":` + leak + `,"gauge":{"dataPoints":[` +
			`{"attributes":[{"key":"k","value":{"kvlistValue":{"values":` + leak + `}}}]}]}}]}]}]}`,
		Traces: `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}}]},` +
			`"scopeSpans":[{"spans":[{"name":"s","links":[{"attributes":` + leak + `}],"unknownField":"LEAK"}]}]}]}`,
	} {
		var m map[string]any
		if err := json.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatal(err)
		}
		if _, err := Redact(signal, m); err != nil {
			t.Fatalf("%s: %v", signal, err)
		}
		out, _ := json.Marshal(m)
		if strings.Contains(string(out), "LEAK") || strings.Contains(string(out), "user.email") {
			t.Errorf("%s: stored %s", signal, out)
		}
	}
}

// Guards: text riding inside an attribute value under a field AnyValue does not have
// ({"stringValue": "ok", "user.email": "..."}), at the top or nested in a list or kvlist.
func TestAnyValueKeepsOnlyItsOwnFields(t *testing.T) {
	doc := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}},` +
		`{"key":"a","value":{"stringValue":"ok","user.email":"LEAK"}},` +
		`{"key":"b","value":{"arrayValue":{"values":[{"intValue":"1","x":"LEAK"}],"y":"LEAK"}}},` +
		`{"key":"c","value":{"kvlistValue":{"values":[{"key":"d","value":{"boolValue":true,"z":"LEAK"}}],"w":"LEAK"}}}]},` +
		`"scopeSpans":[{"spans":[{"name":"s"}]}]}]}`
	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatal(err)
	}
	if _, err := Redact(Traces, m); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(m)
	if strings.Contains(string(out), "LEAK") || !strings.Contains(string(out), `"stringValue":"ok"`) {
		t.Errorf("stored %s", out)
	}
}

// Guards: text stored under a known field name with the wrong type
// ({"stringValue": {"user.email": ...}}, an object where a timestamp belongs) or in metric
// parts the walker did not visit (histogram buckets, summary quantiles).
func TestKnownFieldsHoldOnlyTheirOwnTypes(t *testing.T) {
	for name, c := range map[string]struct{ signal, doc string }{
		"an object as a stringValue": {Logs, `{"resourceLogs":[{"resource":{"attributes":[{"key":"a","value":{"stringValue":{"user.email":"LEAK"}}}]}}]}`},
		"an object as a timestamp":   {Traces, `{"resourceSpans":[{"scopeSpans":[{"spans":[{"name":"s","startTimeUnixNano":{"x":"LEAK"}}]}]}]}`},
		"text in histogram buckets": {Metrics, `{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"exponentialHistogram":{"dataPoints":[` +
			`{"positive":{"offset":1,"bucketCounts":["1"],"note":"LEAK"}}]}}]}]}]}`},
		"text in summary quantiles": {Metrics, `{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"summary":{"dataPoints":[` +
			`{"quantileValues":[{"quantile":0.5,"value":1,"note":"LEAK"}]}]}}]}]}]}`},
	} {
		var m map[string]any
		if err := json.Unmarshal([]byte(c.doc), &m); err != nil {
			t.Fatal(err)
		}
		_, err := Redact(c.signal, m)
		out, _ := json.Marshal(m)
		if err == nil && strings.Contains(string(out), "LEAK") {
			t.Errorf("%s: stored %s", name, out)
		}
	}
}

// Guards: a key-value list inside an allowed attribute carrying what the attribute's own
// rules forbid: a Codex attribute holding a non-allowlisted key, a Claude Code one a content key.
func TestNestedListsFollowTheEnclosingRules(t *testing.T) {
	kv := func(k string) string {
		return `{"key":"model","value":{"kvlistValue":{"values":[{"key":"` + k + `","value":{"stringValue":"LEAK"}}]}}}`
	}
	for name, c := range map[string]struct{ service, key string }{
		"Codex, a key off its allowlist": {"codex_exec", "arguments"},
		"Claude Code, a content key":     {"claude-code", "prompt"},
	} {
		doc := `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"` + c.service + `"}}]},` +
			`"scopeLogs":[{"logRecords":[{"attributes":[` + kv(c.key) + `]}]}]}]}`
		var m map[string]any
		if err := json.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatal(err)
		}
		if _, err := Redact(Logs, m); err != nil {
			t.Fatal(err)
		}
		if out, _ := json.Marshal(m); strings.Contains(string(out), "LEAK") {
			t.Errorf("%s: stored %s", name, out)
		}
	}
}
