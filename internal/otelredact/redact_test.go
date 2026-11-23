package otelredact

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Guards: identity or content reaching storage through Livy's receiver that the collector
// would have removed. The requests and the expected outcome are deploy/otel/prove_redaction.py's
// (captured into testdata), the proof the collector itself passes: no identity key, no marked
// value anywhere, and every metadata value the export exists for still there.
func TestProveRedactionRequests(t *testing.T) {
	raw, err := os.ReadFile("testdata/prove_redaction_requests.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Logs, Metrics, Traces map[string]any
		Identity              []string
		Mark                  string
		Kept                  map[string]string
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	pairs := map[string]bool{}
	var out []byte
	for signal, doc := range map[string]map[string]any{Logs: fx.Logs, Metrics: fx.Metrics, Traces: fx.Traces} {
		if n, err := Redact(signal, doc); err != nil || n == 0 {
			t.Fatalf("%s: %d resources, %v", signal, n, err)
		}
		b, _ := json.Marshal(doc)
		out = append(out, b...)
		flatten(doc, pairs)
	}
	identity := map[string]bool{}
	for _, k := range fx.Identity {
		identity[k] = true
	}
	var problems []string
	for p := range pairs {
		k, v, _ := strings.Cut(p, "=")
		if strings.HasPrefix(v, fx.Mark) {
			problems = append(problems, "leaked: "+k)
		}
		if identity[k] {
			problems = append(problems, "identity key present: "+k)
		}
	}
	if strings.Contains(string(out), fx.Mark) && len(problems) == 0 {
		problems = append(problems, "leaked: a marked value outside any attribute (a body or status message)")
	}
	for k, v := range fx.Kept {
		if !pairs[k+"="+v] {
			problems = append(problems, fmt.Sprintf("dropped wanted attribute %s=%s", k, v))
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

// flatten collects every {"key","value"} attribute as "key=scalar", as prove_redaction does.
func flatten(node any, into map[string]bool) {
	switch n := node.(type) {
	case map[string]any:
		if k, ok := n["key"].(string); ok && len(n) == 2 {
			if v, ok := n["value"].(map[string]any); ok {
				into[k+"="+scalar(v)] = true
			}
		}
		for _, v := range n {
			flatten(v, into)
		}
	case []any:
		for _, v := range n {
			flatten(v, into)
		}
	}
}

func scalar(v map[string]any) string {
	for _, kind := range []string{"stringValue", "intValue", "doubleValue", "boolValue"} {
		if x, ok := v[kind]; ok {
			return fmt.Sprint(x)
		}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// Guards: the collector's rules and Livy's drifting apart, so one path stores what the
// other removes. Each list is read from collector.yaml and compared with the Go one.
func TestRulesMatchCollectorYAML(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/otel/collector.yaml")
	if err != nil {
		t.Fatal(err)
	}
	y := string(raw)
	section := func(name string) string {
		i := strings.Index(y, "  "+name+":\n")
		if i < 0 {
			t.Fatalf("collector.yaml has no %s", name)
		}
		rest := y[i+len(name)+4:]
		if j := regexp.MustCompile(`\n  [a-z]`).FindStringIndex(rest); j != nil {
			rest = rest[:j[0]]
		}
		return rest
	}
	keepIn := func(sec, ctx string) map[string]bool {
		re := regexp.MustCompile(`(?s)- context: ` + ctx + `\n.*?keep_keys\((?:filtered_)?attributes, \[(.*?)\]\)`)
		m := re.FindStringSubmatch(sec)
		if m == nil {
			t.Fatalf("no keep_keys for %s", ctx)
		}
		out := map[string]bool{}
		for _, k := range strings.Split(m[1], ",") {
			out[strings.Trim(strings.TrimSpace(k), `"`)] = true
		}
		return out
	}
	deletes := func(sec string) map[string]bool {
		out := map[string]bool{}
		for _, m := range regexp.MustCompile(`delete_key\(attributes, "([^"]+)"\)`).FindAllStringSubmatch(sec, -1) {
			out[m[1]] = true
		}
		return out
	}
	identity := deletes(section("transform/drop-identity"))
	foreign := section("transform/foreign-content")
	for name, c := range map[string]struct{ yaml, go_ map[string]bool }{
		"identity keys":      {identity, identityKeys},
		"log content":        {deletes(section("transform/drop-log-content")), logContent},
		"span content":       {deletes(section("transform/drop-span-content")), spanContent},
		"foreign log keep":   {keepIn(section("transform/codex-allowlist"), "log"), foreignLogKeep},
		"foreign span keep":  {keepIn(foreign, "span"), foreignSpanKeep},
		"foreign event keep": {keepIn(foreign, "spanevent"), foreignEventKeep},
		"foreign point keep": {keepIn(foreign, "datapoint"), foreignPointKeep},
		"exemplar keep":      {keepIn(foreign, "exemplar"), foreignPointKeep},
	} {
		for k := range c.yaml {
			if !c.go_[k] {
				t.Errorf("%s: %q is in collector.yaml, not in rules.go", name, k)
			}
		}
		for k := range c.go_ {
			if !c.yaml[k] {
				t.Errorf("%s: %q is in rules.go, not in collector.yaml", name, k)
			}
		}
	}
	if !strings.Contains(y, `delete_matching_keys(attributes, "^(user|enduser|host|process)\\.")`) ||
		identityPrefix.String() != `^(user|enduser|host|process)\.` {
		t.Error("identity prefix differs from collector.yaml")
	}
	if !strings.Contains(y, `name != "`+claudeEventKeep+`"`) {
		t.Error("the Claude Code span event kept differs from collector.yaml")
	}
}
