package prompts

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xbew/scrub"

	"github.com/0xbew/livy/internal/extract/claudecode"
	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/oldnames"
)

func denylist(t *testing.T, literal string) *scrub.Denylist {
	t.Helper()
	d, err := scrub.ParseDenylist([]byte(`{"identities":[{"literal":"`+literal+`","role":"owner"}],"vendors":[]}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A name straddling the point where a field is cut must not survive in either half: the
// scrubber has to see the name whole before the cut.
func TestACutNeverSplitsADeniedName(t *testing.T) {
	d := denylist(t, "Zebulon Quux")
	leaks := func(got string) bool {
		low := strings.ToLower(got)
		return strings.Contains(low, "zeb") || strings.Contains(low, "quu")
	}
	for _, lead := range []int{contextChars - 8, contextChars - 10, contextChars - 12} {
		text := strings.Repeat("a ", lead/2) + "Zebulon Quux " + strings.Repeat("b ", 700)
		if got := head(text, d, contextChars); leaks(got) {
			t.Errorf("head with the name at %d kept part of it", lead)
		}
	}
	for _, k := range []int{744, 746, 748} {
		text := "Zebulon Quux " + strings.Repeat("d ", k)
		if got := tail(text, d, contextChars); leaks(got) {
			t.Errorf("tail with %d words after the name kept part of it", k)
		}
	}
}

// The relay rule is the labeller's tested one: at least half of the prompt's 8-word shingles
// from ONE other session, within 14 days. Expected shares are counted by hand.
func TestARelayNeedsHalfTheShinglesFromOneOtherSession(t *testing.T) {
	prompt := "one two three four five six seven eight nine ten" // 3 shingles
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	repo := func(string, bool) string { return "org/repo" }
	cases := []struct {
		name, session, text string
		age                 time.Duration
		want                string
	}{
		{"two of three", "other", "one two three four five six seven eight nine", time.Hour, "yes"},
		{"one of three", "other", "one two three four five six seven eight", time.Hour, "no"},
		{"own session", "own", "one two three four five six seven eight nine ten", time.Hour, "no"},
		{"older than 14 days", "other", "one two three four five six seven eight nine ten", 15 * 24 * time.Hour, "no"},
	}
	for _, c := range cases {
		s := newSources(shingles(prompt))
		s.addText(c.session, now.Add(-c.age), "/x", c.text)
		if got := s.match(prompt, "own", now, repo).Proof; got != c.want {
			t.Errorf("%s: proof %q, want %q", c.name, got, c.want)
		}
	}
}

// Codex writes its own instructions and environment as user messages; only the person's
// words are a prompt.
func TestCodexScaffoldingIsNotAPrompt(t *testing.T) {
	content := []any{
		map[string]any{"type": "input_text", "text": "<environment_context>\n<cwd>/x</cwd>\n</environment_context>"},
		map[string]any{"type": "input_text", "text": "# AGENTS.md instructions for /x\n\nbe brief"},
		map[string]any{"type": "input_text", "text": "fix the failing build"},
	}
	if got := codexText(content, "input_text"); got != "fix the failing build" {
		t.Errorf("prompt %q", got)
	}
	if !codexSubagent(map[string]any{"source": map[string]any{"subagent": map[string]any{}}}) {
		t.Error("a subagent thread is not a person's")
	}
}

// Upload is the engineer's opt-in: off on a fresh machine, and turning it on keeps the
// rest of the config.
func TestUploadIsOffUntilTheEngineerTurnsItOn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LIVY_DIR", filepath.Join(home, "data"))
	if k := oldnames.RecorderEnv("DIR"); k != "" {
		t.Setenv(k, filepath.Join(home, "data"))
	}
	if UploadOn() {
		t.Fatal("a fresh machine has upload on")
	}
	cfg := filepath.Join(home, ".livy", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"scope":{"orgs":["acme"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetUpload(true); err != nil || !UploadOn() {
		t.Fatalf("on: %v", err)
	}
	raw, _ := os.ReadFile(cfg)
	if !strings.Contains(string(raw), `"acme"`) {
		t.Errorf("turning upload on dropped the orgs: %s", raw)
	}
	if err := SetUpload(false); err != nil || UploadOn() {
		t.Fatalf("off: %v", err)
	}
}

// Outside context is judged on the text the source session said, not only on what would
// leave: a redaction inside a passage must not break the match and let it through.
func TestRedactionDoesNotHideAnOutsideRelayInContext(t *testing.T) {
	raw := "alpha beta gamma delta epsilon Zebulon Quux eta theta iota kappa"
	d := denylist(t, "Zebulon Quux")
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	p := claudecode.Prompt{ID: "prompt:x", TS: "2026-11-03T12:00:00Z", Repo: "o/r", SessionID: "own", AgentBefore: raw}
	rec := textRecord(p, d)
	s := newSources(shingles(raw))
	s.addText("elsewhere", now.Add(-time.Hour), "/x", raw)
	outside := func(text string) bool {
		m := s.match(text, "own", now, func(string, bool) string { return Outside })
		return m.Source != nil && *m.Source == Outside
	}
	if n := rec.withholdOutside(p, outside); n != 1 || rec.AgentBefore != Withheld {
		t.Errorf("withheld %d, agent_before %q", n, rec.AgentBefore)
	}
}

// The prompt id store keeps 120 days; a longer backfill would send again what it forgot.
func TestABackfillLongerThanTheIDStoreIsRefused(t *testing.T) {
	days := 121
	got := Run(Options{BackfillDays: &days, DryRun: true, Dir: t.TempDir(), Stderr: io.Discard, Now: time.Now()})
	if _, refused := got["refused"]; !refused {
		t.Errorf("a %d-day backfill ran: %v", days, got)
	}
}

// Guards: a pass killed mid-write leaves prompt-text.jsonl ending in a fragment, and the next
// pass's record glued to it is lost: unreadable to the retry check and to whoever reads the upload.
func TestAnAppendAfterATornLineKeepsItsRecordReadable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	torn := `{"prompt_id":"prompt:a","te`
	if err := os.WriteFile(filepath.Join(dir, TextFile), []byte(torn), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := appendText(dir, []string{`{"prompt_id":"prompt:b","text":"next"}`}); !ok {
		t.Fatal("appendText failed")
	}
	if texts, _ := alreadyWritten(dir); !texts["prompt:b"] {
		raw, _ := os.ReadFile(filepath.Join(dir, TextFile))
		t.Errorf("after a torn line, prompt:b is not readable from %s:\n%s", TextFile, raw)
	}
}

// Guards: a text file whose last line the torn check cannot read, which refused the record
// (a write-only file) or wrote it with nothing said (a failed read).
func TestATextFileTheTornCheckCannotReadStillGetsTheRecordAndSaysWhy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, TextFile)
	if err := os.WriteFile(path, nil, 0o200); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(path); err == nil {
		_ = f.Close()
		t.Skip("a write-only file is readable here (root)")
	}
	ok, unchecked := appendText(dir, []string{`{"prompt_id":"prompt:b","text":"next"}`})
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if texts, _ := alreadyWritten(dir); !ok || !texts["prompt:b"] || unchecked == nil {
		t.Errorf("appendText = %v, unchecked %v; prompt:b readable %v; want written, with why it was unchecked",
			ok, unchecked, texts["prompt:b"])
	}
}

// Guards: a prompt-capture state file that could not be used was taken for a first pass, which
// moved every transcript's offset to its end and saved over the file, so every prompt written
// since the last good pass was skipped with nothing said.
func TestAStateFileThatCannotBeUsedHoldsThePassAndSaysWhy(t *testing.T) {
	cases := []struct {
		name  string
		state string
		mode  os.FileMode
	}{
		{"cut short", `{"version": 1, "files": {`, 0o600},
		{"data after the object", `{"version": 1, "files": {}} {"version": 1, "files": {"a": {`, 0o600},
		{"another version", `{"version": 2, "files": {}, "captured": {}, "last_pass": 0}`, 0o600},
		{"unreadable", `{"version": 1, "files": {}, "captured": {}, "last_pass": 0}`, 0o200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			dir := filepath.Join(home, "data")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, StateName)
			if err := os.WriteFile(path, []byte(c.state), c.mode); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path)
			if err == nil {
				_ = f.Close()
			}
			if err == nil && c.mode == 0o200 {
				t.Skip("a write-only file is readable here (root)")
			}
			Run(Options{Dir: dir, Stderr: io.Discard, Now: time.Now()})
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(path)
			ledgerLines, _ := os.ReadFile(filepath.Join(dir, ledger.FileName))
			if string(got) != c.state || !strings.Contains(string(ledgerLines), `"event":"ledger.error"`) ||
				!strings.Contains(string(ledgerLines), StateName) {
				t.Errorf("state file after the pass = %q, want it left as %q; ledger = %q, want a ledger.error naming %s",
					got, c.state, ledgerLines, StateName)
			}
		})
	}
}
