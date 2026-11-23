package vault

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Guards: prompt text kept past its 90 days, readable by other users, or written as an empty
// record that a later reveal would show as the prompt.
func TestTheVaultKeepsTextNinetyDaysOwnerOnlyAndNeverEmpty(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	sent := time.Date(2026, 8, 1, 23, 30, 0, 0, time.UTC)
	recs := []Record{
		{PromptID: "prompt:a1", HarnessPromptID: "c30aa554-d110-495e-bb8f-38ec842eb63f", SessionID: "s1",
			Repo: "acme/app", Branch: "main", TS: sent, Text: "why does the deploy fail on INC-1?"},
		{PromptID: "prompt:a2", SessionID: "s1", Repo: "acme/app", Branch: "main", TS: sent, Text: "  \n"},
	}
	got, err := Keep(data, recs, sent.Add(time.Hour))
	if err != nil || got.Kept != 1 || got.Empty != 1 {
		t.Fatalf("Keep = %+v, %v; want 1 kept, 1 empty", got, err)
	}
	if again, _ := Keep(data, recs[:1], sent.Add(2*time.Hour)); again.Kept != 0 || again.Already != 1 {
		t.Errorf("Keep of a kept prompt = %+v, want it counted as already kept", again)
	}
	if st, err := os.Stat(filepath.Join(data, Dir)); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("vault folder mode %v (%v), want 0700", st.Mode().Perm(), err)
	}
	files, _ := filepath.Glob(filepath.Join(data, Dir, "*"))
	for _, f := range files {
		if st, _ := os.Stat(f); st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", filepath.Base(f), st.Mode().Perm())
		}
	}
	found, _ := Find(data, []string{"c30aa554-d110-495e-bb8f-38ec842eb63f", "prompt:a2"})
	if len(found) != 1 || found[0].Text != recs[0].Text {
		t.Fatalf("Find by the harness id = %+v, want the one kept record", found)
	}
	// Sent 23:30 on 08-01: still inside 90 days on 10-30 at 23:00, past them on 10-31.
	for _, c := range []struct {
		now  time.Time
		want int
	}{{sent.Add(90*24*time.Hour - time.Hour), 1}, {sent.Add(91 * 24 * time.Hour), 0}} {
		if _, err := Prune(data, c.now); err != nil {
			t.Fatal(err)
		}
		if found, _ := Find(data, []string{"prompt:a1"}); len(found) != c.want {
			t.Errorf("after a prune at %s: %d records, want %d", c.now.Format(time.RFC3339), len(found), c.want)
		}
	}
	if late, _ := Keep(data, recs[:1], sent.Add(91*24*time.Hour)); late.TooOld != 1 {
		t.Errorf("Keep of a prompt sent 91 days ago = %+v, want it refused as too old", late)
	}
}
