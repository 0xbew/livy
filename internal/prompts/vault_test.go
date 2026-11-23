//go:build unix

package prompts

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0xbew/livy/internal/vault"
)

// Guards: prompt text that only the transcript held, lost when the harness cleans it up, and
// a transcript format change read past in silence, losing every prompt after it for good.
func TestAPassKeepsPromptTextAndHoldsAtAUserMessageItCannotRead(t *testing.T) {
	tmp := t.TempDir()
	repo, data := filepath.Join(tmp, "repo"), filepath.Join(tmp, "data")
	for _, args := range [][]string{{"init", "-q", "-b", "main", repo},
		{"-C", repo, "remote", "add", "origin", "https://github.com/acme/app.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Setenv("HOME", tmp)
	t.Setenv("LIVY_SCOPE_ORGS", "acme")
	t.Setenv("LIVY_DIR", data)
	root := filepath.Join(tmp, "projects")
	transcript := filepath.Join(root, "-repo", "s1.jsonl")
	user := func(uuid string, content any, at time.Time) map[string]any {
		return map[string]any{"type": "user", "uuid": uuid, "promptId": "cc-" + uuid, "sessionId": "s1", "cwd": repo,
			"gitBranch": "main", "version": "2.1.319", "timestamp": at.Format(time.RFC3339Nano),
			"message": map[string]any{"role": "user", "content": content}}
	}
	start := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	pass := func(hours int) {
		Run(Options{Roots: []string{root}, Now: start.Add(time.Duration(hours) * time.Hour), Dir: data, Stderr: io.Discard})
	}
	appendLines(t, transcript, user("u0", "before Livy looked", start))
	pass(0) // a machine's first pass starts at the end of every transcript
	appendLines(t, transcript, user("u1", "why does the deploy fail?", start.Add(time.Hour)))
	pass(2)
	if got, _ := vault.Find(data, []string{"cc-u1"}); len(got) != 1 || got[0].Text != "why does the deploy fail?" {
		t.Fatalf("vault after the prompt = %+v, want its text, upload off", got)
	}
	unread := []any{map[string]any{"type": "voice_note", "url": "x"}}
	appendLines(t, transcript, user("u2", unread, start.Add(3*time.Hour)), user("u3", "and now?", start.Add(3*time.Hour)))
	pass(4)
	pass(6)
	if got, _ := vault.Find(data, []string{"prompt:u3"}); len(got) != 0 {
		t.Errorf("a prompt after a user message the reader cannot read was kept: %+v", got)
	}
	raw, _ := os.ReadFile(filepath.Join(data, "ledger.jsonl"))
	if n := strings.Count(string(raw), "format may have changed"); n != 1 {
		t.Errorf("%d ledger.error lines about the format, want 1 (said once, not every pass)", n)
	}
}

func appendLines(t *testing.T, path string, recs ...map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		b, _ := json.Marshal(r)
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// Guards: prompts the transcripts still hold lost to cleanup because the vault began after
// them, a backfill that copies a prompt twice when run again, and one that keeps text past the
// window it was asked for.
func TestAVaultBackfillCopiesWhatTranscriptsHoldOnce(t *testing.T) {
	tmp := t.TempDir()
	repo, data := filepath.Join(tmp, "repo"), filepath.Join(tmp, "data")
	for _, args := range [][]string{{"init", "-q", "-b", "main", repo},
		{"-C", repo, "remote", "add", "origin", "https://github.com/acme/app.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Setenv("HOME", tmp)
	t.Setenv("LIVY_SCOPE_ORGS", "acme")
	t.Setenv("LIVY_DIR", data)
	root := filepath.Join(tmp, "projects")
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	prompt := func(uuid, text string, age time.Duration) map[string]any {
		return map[string]any{"type": "user", "uuid": uuid, "sessionId": "s1", "cwd": repo, "gitBranch": "main",
			"version": "2.1.319", "timestamp": now.Add(-age).Format(time.RFC3339Nano),
			"message": map[string]any{"role": "user", "content": text}}
	}
	appendLines(t, filepath.Join(root, "-repo", "s1.jsonl"),
		prompt("old", "sent 40 days ago", 40*24*time.Hour), prompt("new", "sent yesterday", 24*time.Hour))
	o := Options{Roots: []string{root}, Now: now, Dir: data, Stderr: io.Discard}
	first := VaultBackfill(o, 30)
	if first["kept"] != 1 || first["outside_days"] != 1 {
		t.Errorf("30-day backfill = %v, want 1 kept and 1 outside the days", first)
	}
	if again := VaultBackfill(o, 30); again["kept"] != 0 || again["already"] != 1 {
		t.Errorf("second backfill = %v, want 0 kept and 1 already in the vault", again)
	}
	if got, _ := vault.Find(data, []string{"prompt:new", "prompt:old"}); len(got) != 1 || got[0].Text != "sent yesterday" {
		t.Errorf("vault = %+v, want only the prompt inside the 30 days", got)
	}
	// A line the reader cannot parse may be a prompt: the run is not complete, and says so.
	torn := filepath.Join(root, "-repo", "s2.jsonl")
	if err := os.WriteFile(torn, []byte("{\"type\":\"user\",\"uuid\":\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb strings.Builder
	if code := VaultMain([]string{"backfill", "--days", "30"}, []string{root}, &out, &errb); code != 1 {
		t.Errorf("a backfill that could not read a line exited %d, want 1: %s %s", code, out.String(), errb.String())
	}
	if raw, _ := os.ReadFile(filepath.Join(data, "ledger.jsonl")); !strings.Contains(string(raw), "unreadable") {
		t.Errorf("no ledger.error names the unreadable line: %s", raw)
	}
}

// Guards: a session started in a folder outside every in-scope repo that then commits to one
// (a Claude Desktop scratch workspace) never had its prompts kept, so `livy show --commit` found
// none; one kept with no repo, which `livy show` cannot log a reveal for and so never shows; and
// a session that never touched an in-scope repo kept for being in the same folder.
func TestASessionThatCommitsToAnInScopeRepoIsKeptFromAnyFolder(t *testing.T) {
	tmp := t.TempDir()
	repo, scratch, data := filepath.Join(tmp, "repo"), filepath.Join(tmp, "scratch"), filepath.Join(tmp, "data")
	for _, args := range [][]string{{"init", "-q", "-b", "main", repo},
		{"-C", repo, "remote", "add", "origin", "https://github.com/acme/app.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("LIVY_SCOPE_ORGS", "acme")
	t.Setenv("LIVY_DIR", data)
	root := filepath.Join(tmp, "projects")
	work, idle := filepath.Join(root, "-scratch", "work.jsonl"), filepath.Join(root, "-scratch", "idle.jsonl")
	start := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	prompt := func(session, uuid, text string, at time.Time) map[string]any {
		return map[string]any{"type": "user", "uuid": uuid, "sessionId": session, "cwd": scratch,
			"version": "2.1.319", "timestamp": at.Format(time.RFC3339Nano),
			"message": map[string]any{"role": "user", "content": text}}
	}
	committed := func(dir string) {
		appendLines(t, filepath.Join(dir, "ledger.jsonl"), map[string]any{"event": "commit.recorded",
			"ts": start.Add(2 * time.Hour).Format(time.RFC3339), "repo": "acme/app", "branch": "main",
			"commit_sha": strings.Repeat("a", 40), "harness": "claude-code", "session_id": "work"})
	}
	pass := func(hours int) {
		Run(Options{Roots: []string{root}, Now: start.Add(time.Duration(hours) * time.Hour), Dir: data, Stderr: io.Discard})
	}
	appendLines(t, work, prompt("work", "w0", "look at the repo", start))
	pass(0) // a machine's first pass starts at the end of every transcript
	appendLines(t, work, prompt("work", "w1", "fix the deploy", start.Add(time.Hour)))
	appendLines(t, idle, prompt("idle", "i1", "plan a trip", start.Add(time.Hour)))
	pass(2)
	committed(data)
	appendLines(t, work, prompt("work", "w2", "now push it", start.Add(3*time.Hour)))
	pass(4)
	ids := []string{"prompt:w0", "prompt:w1", "prompt:w2", "prompt:i1"}
	want := []string{"look at the repo", "fix the deploy", "now push it"}
	texts := func(dir string) []string {
		got, _ := vault.Find(dir, ids)
		var out []string
		for _, r := range got {
			out = append(out, r.Text)
			if r.Repo != "acme/app" {
				t.Errorf("%s kept with repo %q, want the repo its session committed to, acme/app", r.PromptID, r.Repo)
			}
		}
		slices.Sort(out)
		return out
	}
	slices.Sort(want)
	if got := texts(data); !slices.Equal(got, want) {
		t.Errorf("vault after the hourly passes = %q, want the committing session's whole text %q", got, want)
	}
	fresh := filepath.Join(tmp, "fresh")
	committed(fresh)
	VaultBackfill(Options{Roots: []string{root}, Now: start.Add(4 * time.Hour), Dir: fresh, Stderr: io.Discard}, 30)
	if got := texts(fresh); !slices.Equal(got, want) {
		t.Errorf("vault after a backfill = %q, want the committing session's whole text %q", got, want)
	}
}
