package install

import (
	"regexp"
	"strconv"
	"strings"
)

// otelLine is a table header or key that configures Codex's otel: [otel], [ "otel".x ],
// otel = {...}, 'otel'.exporter = ..., bare or quoted, any spacing, a trailing comment.
var otelLine = regexp.MustCompile(`^\s*(\[\s*(otel|"otel"|'otel')\s*[.\]]|(otel|"otel"|'otel')\s*[=.])`)

// Codex's config.toml is also rewritten by Codex (it records trusted projects there), so it
// is edited as text and only our own [otel] table is touched: from our marker line to the
// next table header or blank line. A table Codex appends later lands after that.

const codexMarker = "# livy: Codex OTEL export. `livy install` owns the [otel] table below; `livy uninstall` removes it."

// codexBlock is our [otel] table. Prompts, replies, guardian assessments and tool output
// stay off: only the facts Codex logs about each call leave the machine. handle, when set,
// is Codex's `environment`, the one attribute its export carries that says whose it is (the
// collector drops the email). value, when set, is the one export header (Authorization, or
// the relay key): Codex reads export headers only literally.
func codexBlock(endpoint, header, value, handle string, on signals) []string {
	return codexBlockAs(endpoint, header, value, handle, on, "binary")
}

// codexBlockAs is codexBlock with Codex's OTLP protocol: "binary" (protobuf) for a collector,
// "json" for Livy's own receiver, which decodes OTLP/JSON with the standard library.
func codexBlockAs(endpoint, header, value, handle string, on signals, protocol string) []string {
	headers := ""
	if value != "" {
		headers = `, headers = { ` + strconv.Quote(header) + ` = ` + strconv.Quote(value) + ` }`
	}
	block := []string{codexMarker, "[otel]"}
	if handle != "" && tomlSafe(handle) {
		block = append(block, "environment = "+strconv.Quote("engineer.handle="+handle))
	}
	block = append(block,
		"log_user_prompt = false",
		"log_agent_responses = false",
		"log_guardian_assessments = false",
		"tool_result = { max_bytes = 0 }")
	// An off signal is written as "none", never left out: Codex's own default for an
	// unset exporter is not "off" (metrics may go to OpenAI, traces follow the logs one).
	for _, sig := range []struct {
		on        bool
		key, path string
	}{{on.logs, "exporter", "/v1/logs"}, {on.metrics, "metrics_exporter", "/v1/metrics"}, {on.traces, "trace_exporter", "/v1/traces"}} {
		if !sig.on {
			block = append(block, sig.key+` = "none"`)
			continue
		}
		block = append(block, sig.key+` = { otlp-http = { endpoint = `+strconv.Quote(strings.TrimSuffix(endpoint, "/")+sig.path)+
			`, protocol = `+strconv.Quote(protocol)+headers+` } }`)
	}
	return block
}

// signals are the OTEL signals a harness exports.
type signals struct{ logs, metrics, traces bool }

// codexHasForeignOtel reports an [otel] table (or a dotted otel. key) that is not ours.
func codexHasForeignOtel(text string) bool {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if otelLine.MatchString(l) {
			if strings.TrimSpace(l) == "[otel]" && i > 0 && strings.TrimSpace(lines[i-1]) == codexMarker {
				continue
			}
			return true
		}
	}
	return false
}

// withCodexBlock is text with our table replaced by want (or added at the end).
func withCodexBlock(text string, want []string) string {
	stripped := withoutCodexBlock(text)
	if stripped != "" && !strings.HasSuffix(stripped, "\n") {
		stripped += "\n"
	}
	if stripped != "" && !strings.HasSuffix(stripped, "\n\n") {
		stripped += "\n"
	}
	return stripped + strings.Join(want, "\n") + "\n"
}

// withoutCodexBlock removes our table and the blank line that separated it.
func withoutCodexBlock(text string) string {
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == codexMarker {
			start = i
			break
		}
	}
	if start < 0 {
		return text
	}
	end := start + 1
	for end < len(lines) {
		t := strings.TrimSpace(lines[end])
		if t == "" || (strings.HasPrefix(t, "[") && t != "[otel]") {
			break
		}
		end++
	}
	if start > 0 && strings.TrimSpace(lines[start-1]) == "" && (end >= len(lines) || strings.TrimSpace(lines[end]) == "") {
		start--
	}
	out := append(append([]string(nil), lines[:start]...), lines[end:]...)
	return strings.Join(out, "\n")
}

func trimTrailingBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
