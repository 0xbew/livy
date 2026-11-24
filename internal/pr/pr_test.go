package pr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xbew/livy/internal/why"
)

// The spans are the profile package's real fixture (one Claude Code session with a subagent, one
// Codex turn whose children name no session). The ledger lines are written for those sessions:
// two commits by the Claude session (the second amended), one by the Codex session, a checkout,
// a skill load, and another session's lines that no commit names.
const (
	claudeSession = "4de04405-0389-473a-a3b3-c8218e19f63e"
	codexSession  = "01a1e4bc-349d-78a0-b5a6-7bd3716165ea"
	shaA          = "aaaa000000000000000000000000000000000001"
	shaB          = "bbbb000000000000000000000000000000000002"
	shaC          = "cccc000000000000000000000000000000000003"
	shaD          = "dddd000000000000000000000000000000000004"
)

var fixtureLedger = []string{
	`{"event":"work.started","ts":"2026-11-16T14:19:00+00:00","repo":"o/r","branch":"feat/x","hook":"post-checkout"}`,
	`{"event":"session.started","ts":"2026-11-16T14:20:00+00:00","repo":"o/r","branch":"feat/x","session_id":"` + claudeSession + `"}`,
	`{"event":"prompt.submitted","ts":"2026-11-16T14:20:26+00:00","repo":"o/r","branch":"feat/x","session_id":"` + claudeSession + `","prompt_id":"p-1"}`,
	`{"event":"interaction","ts":"2026-11-16T14:20:26.400Z","repo":"o/r","branch":"feat/x","session_id":"` + claudeSession + `","kind":"prompt","interaction_id":"prompt:1"}`,
	`{"event":"usage.api_call","ts":"2026-11-16T14:20:30Z","repo":"o/r","branch":"feat/x","session_id":"` + claudeSession + `","model":"m-1","message_id":"msg-1","request_id":"req-1","input_tokens":5,"output_tokens":7}`,
	`{"event":"tool.outcome","ts":"2026-11-16T14:20:31+00:00","repo":"o/r","branch":"feat/x","session_id":"` + claudeSession + `","tool":"Bash","outcome":"ran"}`,
	`{"event":"skill.loaded","ts":"2026-11-16T14:20:33+00:00","repo":"o/r","branch":"feat/x","session_id":"` + claudeSession + `","skill":"go-development","scope":"project","trigger":"agent","sha256":"ab12","tree_sha256":"cd34"}`,
	`{"event":"commit.recorded","ts":"2026-11-16T14:20:39+00:00","repo":"o/r","branch":"feat/x","session_id":"` + claudeSession + `","commit_sha":"` + shaA + `","prompt_id":"p-1"}`,
	`{"event":"usage.api_call","ts":"2026-11-16T14:20:50Z","repo":"o/r","branch":"feat/x","session_id":"` + claudeSession + `","model":"m-1","message_id":"msg-2","request_id":"req-2","input_tokens":3,"output_tokens":4}`,
	`{"event":"commit.recorded","ts":"2026-11-16T14:20:56+00:00","repo":"o/r","branch":"feat/x","session_id":"` + claudeSession + `","commit_sha":"` + shaB + `"}`,
	`{"event":"commit.rewritten","ts":"2026-11-16T14:21:00+00:00","repo":"o/r","branch":"feat/x","session_id":"` + claudeSession + `","commit_sha":"` + shaC + `","old_commit_sha":"` + shaB + `","rewrite_kind":"amend"}`,
	`{"event":"prompt.captured","ts":"2026-11-16T14:21:30Z","repo":"o/r","branch":"feat/y","session_id":"` + codexSession + `","prompt_id":"c-1"}`,
	`{"event":"commit.recorded","ts":"2026-11-16T14:21:50+00:00","repo":"o/r","branch":"feat/y","session_id":"` + codexSession + `","commit_sha":"` + shaD + `"}`,
	`{"event":"usage.api_call","ts":"2026-11-16T14:20:40Z","repo":"o/r","branch":"main","session_id":"other","model":"m-2","message_id":"msg-9","input_tokens":99}`,
	`{"event":"ledger.error","ts":"2026-11-16T14:20:41+00:00","repo":"unknown","reason":"x"}`,
}

func utc(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Guards: a PR viewer's "What went into this PR" timing out because `why` and `profile` read every
// line on the machine. `livy pr` reads only the index's keys for the PR, and must give the
// answer the full scan gives: same sessions, windows, prompts, tokens, spans, rework and
// skills. The fixture sits in shipped segments, not the live ledger, so only the index carries it.
func TestIndexAnswersAsTheScan(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LIVY_DIR", dir)
	write(t, filepath.Join(dir, "uploaded", "h", "seg.jsonl"), []byte(strings.Join(fixtureLedger, "\n")+"\n"))
	spans, err := os.ReadFile("../profile/testdata/otel-traces.jsonl.gz")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "otel", "uploaded", "h", "seg.jsonl.gz"), spans)
	if _, unread, err := Reindex(dir); err != nil || len(unread) > 0 {
		t.Fatalf("Reindex: %v, unread %v", err, unread)
	}
	facts := []why.Commit{
		{SHA: shaA, Committed: utc(t, "2026-11-16T14:20:39Z"), Sessions: []string{claudeSession}, Parents: 1},
		{SHA: shaC, Committed: utc(t, "2026-11-16T14:20:56Z"), Sessions: []string{claudeSession}, Parents: 1,
			Ancestors: []why.Commit{{SHA: shaA, Committed: utc(t, "2026-11-16T14:20:39Z"), Sessions: []string{claudeSession}}}},
		{SHA: shaD, Committed: utc(t, "2026-11-16T14:21:50Z"), Sessions: []string{codexSession}, Parents: 1},
	}
	o := options{repo: "o/r", branch: "feat/x", top: 20000}
	ld, ok := fromIndex(dir, facts)
	if !ok {
		t.Fatalf("fromIndex: no index (%s)", ld.source.Reason)
	}
	got, want := comparable(t, assemble(o, facts, ld)), comparable(t, assemble(o, facts, fromScan("test")))
	if got != want {
		t.Errorf("livy pr from the index differs from the scan at %s", firstDiff(want, got))
	}
	for _, has := range []string{`"go-development"`, `"p-1"`, `"timed": true`, `"rewritten_commits": 1`} {
		if !strings.Contains(want, has) {
			t.Errorf("the scan's answer lacks %s, so the comparison proves nothing:\n%s", has, want)
		}
	}
}

// comparable is the document as JSON without what says how it was read.
func comparable(t *testing.T, d Doc) string {
	t.Helper()
	d.Source = Source{}
	d.Why.Read = why.JSONRead{}
	d.Profile.Read.Summary, d.Profile.Read.Spans, d.Profile.Read.SpanFiles = "", 0, 0
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// firstDiff is the first line where two documents differ, both ways.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := range min(len(w), len(g)) {
		if w[i] != g[i] {
			return "line " + strconv.Itoa(i+1) + ":\n  scan:  " + w[i] + "\n  index: " + g[i]
		}
	}
	return "the end: scan has " + strconv.Itoa(len(w)) + " lines, index " + strconv.Itoa(len(g))
}
