//go:build unix

package githook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexSession is the real Codex 0.171 session whose rollouts (main and one subagent's,
// cut to session_meta and turn_context) are in testdata/codex.
const codexSession = "01a208be-2398-7d33-ac65-e5108493e4b5"

// codexRepo is commitRepo with that Codex session's shell committing, its rollouts in
// $CODEX_HOME/sessions, and nothing of it left in the ledger (its lines have shipped).
func codexRepo(t *testing.T) (data string) {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("testdata", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	_, data = commitRepo(t)
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", codexSession)
	home := filepath.Join(t.TempDir(), "codex")
	t.Setenv("CODEX_HOME", home)
	day := filepath.Join(home, "sessions", "2026", "11", "23")
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(src, "rollout-*.jsonl"))
	if len(files) != 2 {
		t.Fatalf("want the main and the subagent rollout in %s, got %v", src, files)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(day, filepath.Base(f)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return data
}

// Guards: a Codex commit stamped with no Agent-Model or Agent-Harness and recorded with no
// prompt id, as a real one was: the session's ledger lines had shipped at its last upload,
// and Codex's rollouts were never read. Expected: the model and version the rollouts name,
// and the main thread's newest turn (Codex's prompt id).
func TestCodexCommitReadsTheRollouts(t *testing.T) {
	t.Run("trailers", func(t *testing.T) {
		codexRepo(t)
		msg := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
		if err := os.WriteFile(msg, []byte("probe: livy codex hooks\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := stampFile(msg, "", ""); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(msg)
		for _, want := range []string{"Agent-Model: gpt-5.6-luna", "Agent-Harness: codex@0.171.0", "Agent-Session: " + codexSession} {
			if !strings.Contains(string(got), want+"\n") {
				t.Errorf("message:\n%s\nwant %q", got, want)
			}
		}
	})
	t.Run("commit.recorded", func(t *testing.T) {
		data := codexRepo(t)
		commitT(t, "second")
		PostCommit(nil)
		got := eventsNamed(ledgerLines(t, data), "commit.recorded")
		if len(got) != 1 || got[0]["prompt_id"] != "01a208be-25af-74f2-8fa5-9a746f2d4b0c" || got[0]["harness"] != "codex" {
			t.Errorf("got %v, want one commit.recorded, harness codex, prompt_id the main thread's newest turn", ledgerLines(t, data))
		}
	})
}
