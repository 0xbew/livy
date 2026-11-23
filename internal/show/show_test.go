package show

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/vault"
)

// Guards: text shown with no denylist to scrub it, a name the denylist holds shown to the
// author as shareable, and a reveal that leaves no trace (or leaves the text) in the ledger.
func TestShowScrubsRefusesWithoutADenylistAndLogsIdsOnly(t *testing.T) {
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
	var out, errb bytes.Buffer
	if code := Main([]string{"--prompt", "c30aa554"}, &out, &errb); code == 0 || strings.Contains(out.String(), "deploy") {
		t.Fatalf("with no denylist: exit %d, stdout %q; want a refusal and no text", code, out.String())
	}
	deny := `{"version":1,"identities":[{"literal":"Zebulon Quux","role":"owner"}],"vendors":[]}`
	if err := os.WriteFile(filepath.Join(data, "denylist.json"), []byte(deny), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Main([]string{"--prompt", "c30aa554"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	low := strings.ToLower(out.String())
	if !strings.Contains(low, "why the deploy broke") || strings.Contains(low, "quux") {
		t.Errorf("shown %q: want the scrubbed text, without the denied name", out.String())
	}
	ledger, _ := os.ReadFile(filepath.Join(data, "ledger.jsonl"))
	if !strings.Contains(string(ledger), `"event":"prompt.revealed"`) || !strings.Contains(string(ledger), `"prompt:a1"`) ||
		strings.Contains(string(ledger), "deploy") {
		t.Errorf("ledger %s: want one prompt.revealed naming prompt:a1, with no text", ledger)
	}
}

// Guards: `livy show` exited 0 with fewer prompts than are kept, and called a kept prompt "not in
// the vault", when a folder it reads exists but cannot be read. Only a folder that is not there
// is skipped quietly.
func TestShowNamesAnUnreadableFolderAndExits1(t *testing.T) {
	for _, tc := range []struct {
		name, folder, want string
		args               []string
	}{
		{"vault, --prompt", vault.Dir, "vault could not be read", []string{"--prompt", "c30aa554"}},
		{"spill, --commit", ledger.SpillDir, "spill could not be read", []string{"--commit", "HEAD"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			data := filepath.Join(home, "data")
			t.Setenv("HOME", home)
			t.Setenv("LIVY_DIR", data)
			sent := time.Now().UTC().Add(-time.Hour)
			rec := vault.Record{PromptID: "prompt:a1", HarnessPromptID: "c30aa554", SessionID: "s1", Repo: "acme/app",
				Branch: "main", TS: sent, Text: "why the deploy broke"}
			if _, err := vault.Keep(data, []vault.Record{rec}, sent); err != nil {
				t.Fatal(err)
			}
			deny := `{"version":1,"identities":[{"literal":"Zebulon Quux","role":"owner"}],"vendors":[]}`
			if err := os.WriteFile(filepath.Join(data, "denylist.json"), []byte(deny), 0o600); err != nil {
				t.Fatal(err)
			}
			locked := filepath.Join(data, tc.folder)
			if err := os.MkdirAll(locked, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(locked, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
			if _, err := os.ReadDir(locked); err == nil {
				t.Skip("a 0000 folder is listable here (root, or a file system without modes)")
			}
			var out, errb bytes.Buffer
			code := Main(tc.args, &out, &errb)
			if code != 1 {
				t.Errorf("show %v with an unreadable %s/ exited %d, want 1; stderr %q", tc.args, tc.folder, code, errb.String())
			}
			if all := out.String() + errb.String(); !strings.Contains(all, tc.want) {
				t.Errorf("show %v output does not say %q:\nstdout %s\nstderr %s", tc.args, tc.want, out.String(), errb.String())
			}
		})
	}
}
