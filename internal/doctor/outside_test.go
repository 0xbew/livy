package doctor

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xbew/livy/internal/ledger"
)

// TestOutsideRepoSessionsAreSaid guards a machine whose Claude Code runs in a folder outside every
// in-scope repo and works in one (cd, git -C, a file path): its hooks and the prompt interaction
// are scoped by that folder and never write, while the calls and tools land in the repo, so every
// window reads 0 prompts and doctor said nothing. The fixture is a real claude -p session's
// ledger lines; with its hook lines and prompt interactions taken out (and call.detail's
// prompt_interaction_id, which extraction keeps only for a prompt typed inside a repo) it is that
// machine's shape.
func TestOutsideRepoSessionsAreSaid(t *testing.T) {
	t.Setenv("LIVY_CONFORMANCE_NOW", "1795626000") // 2026-11-25T17:00:00Z, the hour after the fixture
	raw, err := os.ReadFile("../profile/testdata/hook-session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		outside   bool
		wantWarn  bool
		wantInRow string
	}{
		{"started inside the repo", false, false, ""},
		{"started outside the repo", true, true, "1 Claude Code session(s) worked in livy-probe/timing-probe from a folder outside it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ledger.FileName), fixtureLines(t, raw, tc.outside), 0o600); err != nil {
				t.Fatal(err)
			}
			var r Report
			checkOutsideRepo(&r, dir)
			var warned []Row
			for _, row := range r.Rows {
				if row.Status == Warn {
					warned = append(warned, row)
				}
			}
			switch {
			case !tc.wantWarn && len(warned) > 0:
				t.Errorf("rows %+v, want no warning for a session whose prompts were recorded", warned)
			case tc.wantWarn && (len(warned) != 1 || !strings.Contains(warned[0].Detail, tc.wantInRow)):
				t.Errorf("rows %+v, want one warning saying %q", r.Rows, tc.wantInRow)
			}
		})
	}
}

// fixtureLines is raw's ledger lines, as a session started outside the repo leaves them when outside.
func fixtureLines(t *testing.T, raw []byte, outside bool) []byte {
	t.Helper()
	var out bytes.Buffer
	for line := range bytes.SplitSeq(bytes.TrimSpace(raw), []byte("\n")) {
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatal(err)
		}
		if outside {
			switch rec["event"] {
			case "prompt.submitted", "turn.ended", "interaction", "session.started", "tool.call":
				continue
			}
			delete(rec, "prompt_interaction_id")
		}
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(append(b, '\n'))
	}
	return out.Bytes()
}
