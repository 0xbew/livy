package relay

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain points HOME at an empty folder: the receiver reads otel.ignore from
// ~/.livy/config.json on every export, and a developer's own list made these tests drop the
// exports they assert are stored. A test that needs a config sets HOME itself.
func TestMain(m *testing.M) {
	os.Exit(runWithEmptyHome(m))
}

func runWithEmptyHome(m *testing.M) int {
	home, err := os.MkdirTemp("", "relay-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "relay tests: no empty HOME:", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(home) }() // a leftover temp folder fails no test
	if err := os.Setenv("HOME", home); err != nil {
		fmt.Fprintln(os.Stderr, "relay tests: HOME not set:", err)
		return 2
	}
	return m.Run()
}

// Guards: Livy's receiver storing an export unredacted (identity reaching the bucket), a
// protobuf export accepted and lost without a word (Livy can only redact OTLP/JSON), an empty
// probe written as data, and the size cap dropping exports silently.
func TestSinkStoresOnlyRedactedOTLPJSON(t *testing.T) {
	dir := t.TempDir()
	s := &Store{Dir: dir}
	export := `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}},` +
		`{"key":"user.email","value":{"stringValue":"eng@example.com"}}]},"scopeLogs":[{"logRecords":[{"attributes":[` +
		`{"key":"event.name","value":{"stringValue":"api_request"}},{"key":"prompt","value":{"stringValue":"secret plans"}}]}]}]}]}`
	json := http.Header{"Content-Type": {"application/json"}}
	for _, c := range []struct {
		name         string
		body         string
		header       http.Header
		status       int
		stored, prob bool
	}{
		{"an export", export, json, 200, true, false},
		{"protobuf", "\x0a\x00", http.Header{"Content-Type": {"application/x-protobuf"}}, 415, false, true},
		{"an empty probe", `{"resourceLogs":[]}`, json, 200, false, false},
		{"not JSON", "{", json, 400, false, true},
	} {
		status, _, _, stored, _, problem := s.Write(LogsPath, []byte(c.body), c.header)
		if status != c.status || stored != c.stored || (problem != "") != c.prob {
			t.Errorf("%s: HTTP %d stored %v problem %q; want HTTP %d stored %v problem %v", c.name, status, stored, problem, c.status, c.stored, c.prob)
		}
	}
	line, _ := os.ReadFile(filepath.Join(dir, "ledger.jsonl"))
	if strings.Count(string(line), "\n") != 1 || strings.Contains(string(line), "eng@example.com") ||
		strings.Contains(string(line), "secret plans") || !strings.Contains(string(line), "api_request") {
		t.Errorf("stored stream:\n%s", line)
	}
	s.MaxBytes = int64(len(line)) + 10
	if status, _, _, stored, dropped, problem := s.Write(LogsPath, []byte(export), json); status == 200 || stored || !dropped || problem == "" {
		t.Errorf("over the cap: HTTP %d stored %v dropped %v problem %q; want refused, dropped and said", status, stored, dropped, problem)
	}
}

// Guards: an approval or refusal Claude Code made (OTEL's tool_decision, the only record of an
// approval granted) lost once emit ships and deletes the otel stream, so `livy why` can never
// say what was auto-approved. The export is one a real Claude Code session sent.
func TestToolDecisionsAreKeptBesideTheStream(t *testing.T) {
	data := t.TempDir()
	s := &Store{Dir: filepath.Join(data, OtelDir)}
	body, err := os.ReadFile(filepath.Join("testdata", "claude-code-logs-tool-decisions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if status, _, _, stored, _, problem := s.Write(LogsPath, body, http.Header{"Content-Type": {"application/json"}}); status != 200 || !stored {
		t.Fatalf("HTTP %d stored %v (%s)", status, stored, problem)
	}
	files, _ := filepath.Glob(filepath.Join(data, DecisionsDir, "*.jsonl"))
	var got []string
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		got = append(got, strings.Split(strings.TrimSpace(string(raw)), "\n")...)
	}
	want := []string{
		`"decision":"accept","decision_source":"config","event":"tool.decision"`,
		`"decision":"reject","decision_source":"config","event":"tool.decision"`,
	}
	if len(got) != len(want) {
		t.Fatalf("%d decision lines, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) || !strings.Contains(got[i], `"prompt_id":"585c97c8-f3b4-44e0-809b-2a856ba05b62"`) ||
			!strings.Contains(got[i], `"session_id":"0d7cc70e-85ed-48fb-886b-8f206db58614"`) || !strings.Contains(got[i], `"tool":"Bash"`) {
			t.Errorf("line %d = %s, want %s for prompt 585c97c8 in session 0d7cc70e, tool Bash", i, got[i], w)
		}
	}
}

// Guards: a decision file whose last line the torn check cannot read, which refused the
// decisions (a write-only file) or kept them with nothing said (a failed read).
func TestDecisionsAreKeptWhenTheTornCheckCannotReadAndItIsSaid(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	s := &Store{Dir: filepath.Join(data, OtelDir)}
	body, err := os.ReadFile(filepath.Join("testdata", "claude-code-logs-tool-decisions.json"))
	if err != nil {
		t.Fatal(err)
	}
	day := filepath.Join(data, DecisionsDir, time.Now().UTC().Format("2006-01-02")+".jsonl")
	if err := os.MkdirAll(filepath.Dir(day), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(day, nil, 0o200); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(day); err == nil {
		_ = f.Close()
		t.Skip("a write-only file is readable here (root)")
	}
	_, _, _, stored, _, problem := s.Write(LogsPath, body, http.Header{"Content-Type": {"application/json"}})
	if err := os.Chmod(day, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(day)
	if n := strings.Count(string(raw), `"event":"tool.decision"`); !stored || n != 2 || !strings.Contains(problem, "torn-line check") {
		t.Errorf("stored %v, %d decisions kept, problem %q; want both kept and the unchecked torn line said", stored, n, problem)
	}
}

// Guards: kept decisions outliving their 90 days on a machine whose exports stop carrying any.
func TestOldDecisionsArePrunedWithoutNewOnes(t *testing.T) {
	data := t.TempDir()
	s := &Store{Dir: filepath.Join(data, OtelDir)}
	old := filepath.Join(data, DecisionsDir, time.Now().UTC().AddDate(0, 0, -100).Format("2006-01-02")+".jsonl")
	if err := os.MkdirAll(filepath.Dir(old), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	export := `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"attributes":[{"key":"event.name","value":{"stringValue":"api_request"}}]}]}]}]}`
	if status, _, _, _, _, problem := s.Write(LogsPath, []byte(export), http.Header{"Content-Type": {"application/json"}}); status != 200 {
		t.Fatalf("HTTP %d (%s)", status, problem)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("a decision file 100 days old is still there after an export with no decisions")
	}
}

// Guards: OTEL from a project the user listed in otel.ignore stored and shipped anyway (the
// receiver stored every export), a glob folder entry written through a symlink missing the
// resolved folder a session reports, and a Codex record that names no conversation kept although
// it may come from a listed project. Both exports are ones a real Claude Code and Codex sent;
// the rollout line is Codex's session_meta shape with the folder swapped.
func TestIgnoredProjectsLeaveNothingInTheStream(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LIVY_DIR", filepath.Join(home, ".livy"))
	if err := os.MkdirAll(filepath.Join(home, ".livy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".livy", "config.json"), []byte(`{"otel":{"ignore":["~/personal","~/link/side-*"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	claude, err := os.ReadFile(filepath.Join("testdata", "claude-code-logs-tool-decisions.json"))
	if err != nil {
		t.Fatal(err)
	}
	codex, err := os.ReadFile(filepath.Join("testdata", "codex-logs-conversation.json"))
	if err != nil {
		t.Fatal(err)
	}
	personal, work := filepath.Join(home, "personal", "app"), filepath.Join(home, "work", "app")
	// A session reports its folder with symlinks resolved, as macOS reports /tmp as /private/tmp.
	if err := os.MkdirAll(filepath.Join(home, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "real"), filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, folder string
		codex        bool
		records      int // log records stored
	}{
		{"claude code in a listed folder", personal, false, 0},
		{"claude code elsewhere", work, false, 13},
		{"claude code in a glob folder listed through a symlink", filepath.Join(home, "real", "side-app"), false, 0},
		{"codex in a listed folder", personal, true, 0},
		{"codex elsewhere: its records naming no conversation dropped", work, true, 4},
	} {
		if err := os.MkdirAll(c.folder, 0o700); err != nil {
			t.Fatal(err)
		}
		in := http.Header{"Content-Type": {"application/json"}}
		body := claude
		if c.codex {
			body = codex
			codexHome := filepath.Join(t.TempDir(), "codex")
			t.Setenv("CODEX_HOME", codexHome)
			id := "01a20920-980e-7583-8825-d14db53c8d4d"
			day := filepath.Join(codexHome, "sessions", "2026", "11", "23")
			if err := os.MkdirAll(day, 0o700); err != nil {
				t.Fatal(err)
			}
			meta := `{"timestamp":"2026-11-23T15:57:41.630Z","type":"session_meta","payload":{"id":"` + id + `","cwd":"` + c.folder + `"}}` + "\n"
			if err := os.WriteFile(filepath.Join(day, "rollout-2026-11-23T10-57-41-"+id+".jsonl"), []byte(meta), 0o600); err != nil {
				t.Fatal(err)
			}
		} else {
			in.Set("x-livy-cwd", url.PathEscape(c.folder))
		}
		dir := filepath.Join(t.TempDir(), OtelDir)
		s := &Store{Dir: dir}
		if status, _, _, _, _, problem := s.Write(LogsPath, body, in); status != 200 {
			t.Fatalf("%s: HTTP %d (%s)", c.name, status, problem)
		}
		raw, _ := os.ReadFile(filepath.Join(dir, "ledger.jsonl"))
		if got := strings.Count(string(raw), `"observedTimeUnixNano"`); got != c.records {
			t.Errorf("%s: %d log records stored, want %d", c.name, got, c.records)
		}
	}
}
