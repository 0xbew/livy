//go:build unix

package codex

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xbew/livy/internal/extract"
)

func rollout(t *testing.T, path string, recs ...map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	var b []byte
	for _, r := range recs {
		line, _ := json.Marshal(r)
		b = append(append(b, line...), '\n')
	}
	if err := os.WriteFile(path, b, 0o666); err != nil {
		t.Fatal(err)
	}
}

func call(id string) map[string]any {
	u := map[string]any{"input_tokens": 10, "cached_input_tokens": 4, "cache_write_input_tokens": 0,
		"output_tokens": 3, "reasoning_output_tokens": 1, "total_tokens": 13}
	return map[string]any{"type": "token_usage_record", "timestamp": "2026-11-01T10:00:00.000Z",
		"payload": map[string]any{"response_id": id, "turn_id": "t1", "session_id": "s1", "usage": u}}
}

// TestFormatDriftHoldsTheRolloutAndSaysSoOnce guards contract rule 6 for Codex: a record type
// or usage shape this reader does not know must write one ledger.error naming the Codex
// version and stop the file before it, never pass over it and read on in silence.
func TestFormatDriftHoldsTheRolloutAndSaysSoOnce(t *testing.T) {
	for name, bad := range map[string]map[string]any{
		"unknown record type": {"type": "brand_new_kind", "timestamp": "2026-11-01T10:00:00.000Z", "payload": map[string]any{}},
		"usage renamed": {"type": "token_usage_record", "timestamp": "2026-11-01T10:00:00.000Z",
			"payload": map[string]any{"response_id": "r-bad", "turn_id": "t1", "usage_v2": map[string]any{}}},
		"token_count info of the wrong type": {"type": "event_msg", "timestamp": "2026-11-01T10:00:00.000Z",
			"payload": map[string]any{"type": "token_count", "info": "oops"}},
	} {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			repo := filepath.Join(tmp, "repo")
			for _, args := range [][]string{{"init", "-q", "-b", "main", repo},
				{"-C", repo, "remote", "add", "origin", "https://github.com/acme/app.git"}} {
				if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}
			t.Setenv("HOME", tmp)
			t.Setenv("LIVY_SCOPE_ORGS", "acme")
			root, data := filepath.Join(tmp, "sessions"), filepath.Join(tmp, "data")
			rollout(t, filepath.Join(root, "2026", "11", "01", "rollout-x.jsonl"),
				map[string]any{"type": "session_meta", "payload": map[string]any{"id": "s1", "cli_version": "0.999.0"}},
				map[string]any{"type": "turn_context", "payload": map[string]any{"turn_id": "t1", "cwd": repo, "model": "m"}},
				call("r-1"), bad, call("r-2"))
			for range 2 {
				Run(root, data, &extract.Writer{Dir: data, Stderr: io.Discard}, io.Discard)
			}
			var calls, errs []string
			f, err := os.Open(filepath.Join(data, "ledger.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				var l map[string]any
				_ = json.Unmarshal(sc.Bytes(), &l)
				switch l["event"] {
				case "usage.api_call":
					calls = append(calls, l["message_id"].(string))
				case "ledger.error":
					errs = append(errs, l["reason"].(string))
				}
			}
			if strings.Join(calls, ",") != "r-1" {
				t.Errorf("usage lines for %v, want only r-1, the call before the unrecognised record", calls)
			}
			if len(errs) != 1 || !strings.Contains(errs[0], "Codex 0.999.0") {
				t.Errorf("errors %q, want one naming Codex 0.999.0 across two runs", errs)
			}
		})
	}
}

// TestCallIsNotWrittenAgainAfterACutShortPass guards contract rule 6 for Codex: a pass killed
// after appending its lines but before saving its offsets must not write them again.
func TestCallIsNotWrittenAgainAfterACutShortPass(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	for _, args := range [][]string{{"init", "-q", "-b", "main", repo},
		{"-C", repo, "remote", "add", "origin", "https://github.com/acme/app.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Setenv("HOME", tmp)
	t.Setenv("LIVY_SCOPE_ORGS", "acme")
	root, data := filepath.Join(tmp, "sessions"), filepath.Join(tmp, "data")
	rollout(t, filepath.Join(root, "2026", "11", "01", "rollout-x.jsonl"),
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": "s1", "cli_version": "0.164.0"}},
		map[string]any{"type": "turn_context", "payload": map[string]any{"turn_id": "t1", "cwd": repo, "model": "m"}},
		call("r-1"))
	Run(root, data, &extract.Writer{Dir: data, Stderr: io.Discard}, io.Discard)
	if err := os.Remove(filepath.Join(data, stateName)); err != nil { // the offsets never reached disk
		t.Fatal(err)
	}
	Run(root, data, &extract.Writer{Dir: data, Stderr: io.Discard}, io.Discard)
	raw, _ := os.ReadFile(filepath.Join(data, "ledger.jsonl"))
	if n := strings.Count(string(raw), `"event":"usage.api_call"`); n != 1 {
		t.Errorf("r-1 written %d times across two runs, want once", n)
	}
}

// Guards: a Codex extraction state that could not be used was taken for an empty one and saved
// over, so the next pass read every rollout from byte 0 and wrote every call older than the
// written log's window again as a duplicate usage.api_call, with nothing said.
func TestAStateFileThatCannotBeUsedHoldsThePassAndSaysWhy(t *testing.T) {
	cases := []struct {
		name  string
		state string
		mode  os.FileMode
	}{
		{"cut short", `{"files": {"x": 1`, 0o600},
		{"not an object", `[]`, 0o600},
		{"unreadable", `{"files": {}}`, 0o200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := t.TempDir()
			path := filepath.Join(data, stateName)
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
			Run(filepath.Join(data, "sessions"), data, &extract.Writer{Dir: data, Stderr: io.Discard}, io.Discard)
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(path)
			ledgerLines, _ := os.ReadFile(filepath.Join(data, "ledger.jsonl"))
			if string(got) != c.state || !strings.Contains(string(ledgerLines), `"event":"ledger.error"`) ||
				!strings.Contains(string(ledgerLines), stateName) {
				t.Errorf("state file after the pass = %q, want it left as %q; ledger = %q, want a ledger.error naming %s",
					got, c.state, ledgerLines, stateName)
			}
		})
	}
}
