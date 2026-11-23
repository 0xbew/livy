//go:build unix

package claudecode

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var backfillNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

func backfill(t *testing.T, root, data string) Stats {
	t.Helper()
	return ToolBackfill(Options{Roots: []string{root}, Dir: data, Stderr: io.Discard, Now: backfillNow})
}

func toolCalls(t *testing.T, data string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, l := range ledgerLines(t, data) {
		if l["event"] == "tool.call" {
			out[l["tool_use_id"].(string)] = l
		}
	}
	return out
}

func settle(t *testing.T, path string) {
	t.Helper()
	old := backfillNow.Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// TestToolCallLinesMatchThePythonRecorder guards the backfill drifting from the Python
// recorder's lines, which derive already reads: a result stamped before its call, a call with
// no result, a result older than its call, a spawned agent, and input sizes measured as
// Python's compact UTF-8 JSON (non-ASCII, escapes, float spelling). The expected lines are
// that recorder's own output on the same transcript.
func TestToolCallLinesMatchThePythonRecorder(t *testing.T) {
	repo, root, data := setup(t)
	raw, err := os.ReadFile("testdata/toolcalls/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "-p", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(raw), "{{CWD}}", repo)), 0o666); err != nil {
		t.Fatal(err)
	}
	settle(t, path)
	backfill(t, root, data)
	if again := backfill(t, root, data); again["written"] != 0 {
		t.Errorf("a second run wrote %d lines, want 0", again["written"])
	}
	golden := t.TempDir()
	if b, err := os.ReadFile("testdata/toolcalls/python_recorder.jsonl"); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(filepath.Join(golden, "ledger.jsonl"), b, 0o666); err != nil {
		t.Fatal(err)
	}
	want, got := toolCalls(t, golden), toolCalls(t, data)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tool.call lines differ from the Python recorder's:\n got  %v\n want %v", got, want)
	}
	ledger, _ := os.ReadFile(filepath.Join(data, "ledger.jsonl"))
	if strings.Contains(string(ledger), "PLACEHOLDER") {
		t.Error("a tool's input or result text reached the ledger")
	}
	reported := false
	for _, l := range ledgerLines(t, data) {
		reported = reported || (l["event"] == "ledger.error" && strings.Contains(l["reason"].(string), "stamped before their call"))
	}
	if !reported {
		t.Error("a result stamped before its call was dropped without a ledger.error")
	}
}

// TestFileWithAYoungUnansweredCallWaitsForItsResult guards a long-running agent call being
// recorded as no_result for good: the file is held, its offset does not move, and the call is
// written once, with its result, when that arrives.
func TestFileWithAYoungUnansweredCallWaitsForItsResult(t *testing.T) {
	repo, root, data := setup(t)
	at := backfillNow.Add(-time.Hour).Format("2006-01-02T15:04:05.000Z")
	path := filepath.Join(root, "-p", "s1.jsonl")
	call := map[string]any{"type": "assistant", "cwd": repo, "gitBranch": "feat", "sessionId": "s1", "timestamp": at,
		"message": map[string]any{"id": "m-1", "stop_reason": "tool_use", "content": []any{
			map[string]any{"type": "tool_use", "id": "t-run", "name": "Agent", "input": map[string]any{}}}}}
	writeLines(t, path, call)
	settle(t, path)
	if s := backfill(t, root, data); s["held_files"] != 1 || len(toolCalls(t, data)) != 0 {
		t.Fatalf("stats %v, calls %v: want the file held and nothing written", s, toolCalls(t, data))
	}
	writeLines(t, path, map[string]any{"type": "user", "cwd": repo, "timestamp": at, "message": map[string]any{
		"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t-run", "content": "x"}}}})
	settle(t, path)
	backfill(t, root, data)
	backfill(t, root, data)
	got := toolCalls(t, data)
	if len(got) != 1 || got["t-run"]["result"] != "ok" {
		t.Errorf("tool calls %v, want t-run once with result ok", got)
	}
}
