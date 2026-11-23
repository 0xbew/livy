package doctor

import (
	"errors"
	"strings"
	"testing"
)

// Guards: a collector that refuses this machine's export reading as healthy. Before this,
// any HTTP answer was OK, so a writer that could not be acted as (401/403) or a collector
// with no route (404) passed doctor while no telemetry arrived.
func TestOtelProbeVerdict(t *testing.T) {
	for _, c := range []struct {
		status int
		err    error
		want   string
	}{
		{200, nil, OK},
		{401, nil, Fail},
		{403, nil, Fail},
		{404, nil, Fail},
		{500, nil, Fail},
		{0, errors.New("dial tcp: no route to host"), Warn},
	} {
		if got, _ := OtelProbeVerdict(c.status, c.err); got != c.want {
			t.Errorf("HTTP %d, err %v: %s, want %s", c.status, c.err, got, c.want)
		}
	}
}

// Guards: prompt, reply or tool text leaving the machine because a content flag was set in
// the shell or settings, and a signal routed past the collector or off OTLP/HTTP, all while
// the export probe itself passes.
func TestOtelSettingsProblems(t *testing.T) {
	for _, c := range []struct {
		name     string
		settings map[string]any
		shell    map[string]string
		want     []string // failing rows, in order
	}{
		{"installed values only", map[string]any{"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf"}, nil, nil},
		{"prompts on in the shell", nil, map[string]string{"OTEL_LOG_USER_PROMPTS": "1"}, []string{"otel content"}},
		{"a flag set to off", map[string]any{"OTEL_LOG_TOOL_DETAILS": "false"}, map[string]string{"OTEL_LOG_USER_PROMPTS": " OFF "}, nil},
		{"detailed tracing", map[string]any{"BETA_TRACING_ENDPOINT": "https://elsewhere"}, nil, []string{"otel content"}},
		{"logs sent elsewhere", nil, map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "https://elsewhere"}, []string{"otel endpoint"}},
		{"traces over gRPC", map[string]any{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc"}, nil, []string{"otel protocol"}},
		{"settings turns off what the shell turns on", map[string]any{"OTEL_LOG_USER_PROMPTS": "0"},
			map[string]string{"OTEL_LOG_USER_PROMPTS": "1"}, nil},
		{"the settings value wins over the shell's", map[string]any{"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "http/json"},
			map[string]string{"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "grpc"}, nil},
	} {
		rows := OtelSettingsProblems(c.settings, func(k string) string { return c.shell[k] })
		var got []string
		for _, r := range rows {
			if r.Status == Fail {
				got = append(got, r.Check)
			}
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: failing rows %v, want %v", c.name, got, c.want)
		}
	}
}

// Guards: content capture set as a JSON boolean or number in settings going unreported.
// Claude Code passes such a value on as text ("true", "1"), so it is on.
func TestOtelContentFlagOfAnyJSONType(t *testing.T) {
	for _, v := range []any{true, 1} {
		rows := OtelSettingsProblems(map[string]any{"OTEL_LOG_USER_PROMPTS": v}, func(string) string { return "" })
		if len(rows) != 1 || rows[0].Check != "otel content" || rows[0].Status != Fail {
			t.Errorf("OTEL_LOG_USER_PROMPTS=%v in settings: rows %v, want one failing otel content row", v, rows)
		}
	}
}
