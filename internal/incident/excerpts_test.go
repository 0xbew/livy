package incident

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xbew/livy/internal/vault"
	"github.com/0xbew/livy/internal/why"
)

// Guards: an incident pack carrying prompt text that skipped `livy show`'s rules: unscrubbed,
// shown with no denylist, or revealed with no ledger trace of which prompts and which commit.
func TestExcerptsGoThroughShow(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, "data")
	t.Setenv("HOME", home)
	t.Setenv("LIVY_DIR", data)
	sent := time.Now().UTC().Add(-time.Hour)
	rec := vault.Record{PromptID: "prompt:a1", HarnessPromptID: "c30aa554", SessionID: "s1", Repo: "acme/app",
		Branch: "main", TS: sent, Text: "ask Zebulon Quux why the deploy broke"}
	if _, err := vault.Keep(data, []vault.Record{rec}, sent); err != nil {
		t.Fatal(err)
	}
	commits := []why.Explained{{Commit: why.Commit{SHA: "aaaaaaaaaaaaaaaa"},
		Report: why.Report{SHA: "aaaaaaaaaaaaaaaa", Sessions: []why.Session{{ID: "s1", Prompts: []string{"c30aa554"}}}}}}
	if got, err := excerpts(commits, io.Discard); err == nil || strings.Contains(got, "deploy") {
		t.Fatalf("with no denylist: %q, %v; want a refusal and no text", got, err)
	}
	deny := `{"version":1,"identities":[{"literal":"Zebulon Quux","role":"owner"}],"vendors":[]}`
	if err := os.WriteFile(filepath.Join(data, "denylist.json"), []byte(deny), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := excerpts(commits, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if low := strings.ToLower(got); !strings.Contains(low, "why the deploy broke") || strings.Contains(low, "quux") {
		t.Errorf("excerpts %q: want the scrubbed text without the denied name", got)
	}
	ledger, _ := os.ReadFile(filepath.Join(data, "ledger.jsonl"))
	if !strings.Contains(string(ledger), `"commit_sha":"aaaaaaaaaaaaaaaa"`) || !strings.Contains(string(ledger), `"prompt:a1"`) ||
		strings.Contains(string(ledger), "deploy") {
		t.Errorf("ledger %s: want a prompt.revealed naming prompt:a1 and the commit, with no text", ledger)
	}
}
