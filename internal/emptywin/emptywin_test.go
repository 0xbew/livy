//go:build unix

package emptywin

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xbew/livy/internal/extract/claudecode"
	"github.com/0xbew/livy/internal/why"
)

// session is shaped like the owner's: one commit whose window holds no model call line.
const session = "b274f0e7-0000-4000-8000-000000000000"

// TestEmptyWindowSaysWhy guards a PR page of zeros ("0 prompts, 0 tool calls, 0 model calls, no API
// calls recorded in the window") for a commit made in an agent session, with nothing saying whether
// the calls are unread, out of scope, never made, or from a harness Livy does not read. Each row
// is a transcript as Claude Code writes it (or none), with the extractor run or not, as on a
// machine.
func TestEmptyWindowSaysWhy(t *testing.T) {
	from, to := at(t, "2026-11-25T10:00:00Z"), at(t, "2026-11-25T10:05:00Z")
	in, before := "2026-11-25T10:02:00.000Z", "2026-11-25T09:58:00.000Z"
	for name, tc := range map[string]struct {
		harness  string
		call     string // the transcript's one call's time; "" for no transcript
		extracts bool   // the extractor ran over the transcript
		want     string
	}{
		"calls in the window, transcript not read yet":         {"claude-code", in, false, KindNotRead},
		"calls in the window read, worked outside every repo":  {"claude-code", in, true, KindOutOfScope},
		"no call in the window (a commit by a hook or script)": {"claude-code", before, true, KindNoCalls},
		"no transcript on this machine":                        {"", "", false, KindNoTranscript},
		"a Codex session":                                      {"codex", "", false, KindHarness},
	} {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("HOME", tmp)
			t.Setenv("LIVY_SCOPE_ORGS", "acme")
			root, data := filepath.Join(tmp, "projects"), filepath.Join(tmp, "data")
			if tc.call != "" {
				outside := filepath.Join(tmp, "outside")
				writeCall(t, filepath.Join(root, "-p", session+".jsonl"), outside, tc.call)
			}
			if tc.extracts {
				claudecode.Run(claudecode.Options{Roots: []string{root}, Dir: data, Stderr: io.Discard, Now: time.Now().Add(time.Hour)})
			}
			got := New(data, []string{root}).Explain(why.Session{ID: session, Harness: tc.harness, From: from, To: to})
			if got.Kind != tc.want || got.Reason == "" {
				t.Errorf("explained %+v, want kind %s with a reason", got, tc.want)
			}
		})
	}
}

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// writeCall writes a transcript holding one model call made at ts from cwd.
func writeCall(t *testing.T, path, cwd, ts string) {
	t.Helper()
	rec := map[string]any{"type": "assistant", "cwd": cwd, "sessionId": session, "version": "2.1.0", "timestamp": ts,
		"message": map[string]any{"id": "msg_1", "model": "claude-opus-5-5", "content": []any{map[string]any{"type": "text", "text": "x"}},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 2}}}
	b, _ := json.Marshal(rec)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o666); err != nil {
		t.Fatal(err)
	}
}
