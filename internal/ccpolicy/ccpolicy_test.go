package ccpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Guards: Claude Code's managed settings taking Livy's OTEL route away while doctor said ok
// (#225). A managed otelHeadersHelper meant no Claude Code span reached Livy's receiver for
// eleven days; a managed http/protobuf, which the receiver decodes, and a managed metrics
// endpoint do not take it; gRPC does.
func TestOverridesFromManagedFile(t *testing.T) {
	issueShaped := `{"otelHeadersHelper": "/opt/vendor/otel-headers.sh",
		"env": {"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
			"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "https://metrics.vendor.example/v1/metrics"}}`
	for _, c := range []struct {
		name    string
		managed string // "" is no managed file
		want    string // key:breaks, in order
	}{
		{"no managed file", "", ""},
		{"managed helper, protobuf and a metrics endpoint", issueShaped,
			"otelHeadersHelper:true OTEL_EXPORTER_OTLP_METRICS_ENDPOINT:false"},
		{"managed gRPC", `{"env": {"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}}`, "OTEL_EXPORTER_OTLP_PROTOCOL:true"},
	} {
		dir := t.TempDir()
		if c.managed != "" {
			if err := os.WriteFile(filepath.Join(dir, "managed-settings.json"), []byte(c.managed), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		s, err := Read(dir)
		if err != nil {
			t.Fatalf("%s: Read: %v", c.name, err)
		}
		var got []string
		for _, o := range s.Overrides() {
			got = append(got, o.Key+":"+map[bool]string{true: "true", false: "false"}[o.Breaks])
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%s: overrides %q, want %q", c.name, strings.Join(got, " "), c.want)
		}
	}
}
