//go:build unix

package githook

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// commitRepo is an in-scope repo on branch feature/x with one commit, the test running inside
// it with Livy's data folder under the test's temp dir and a Claude Code session s-1 committing.
func commitRepo(t *testing.T) (repo, data string) {
	t.Helper()
	root := t.TempDir()
	repo = filepath.Join(root, "repo")
	data = filepath.Join(root, "data")
	t.Setenv("HOME", root)
	t.Setenv("LIVY_DIR", data)
	t.Setenv("LIVY_SCOPE_ORGS", "acme")
	t.Setenv("LIVY_GIT_TIMEOUT", "")
	_ = os.Unsetenv("LIVY_GIT_TIMEOUT")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s-1")
	t.Setenv("CODEX_THREAD_ID", "")
	_ = os.Unsetenv("CODEX_THREAD_ID")
	gitT(t, "init", "-q", "-b", "main", repo)
	t.Chdir(repo)
	gitT(t, "remote", "add", "origin", "git@github.com:acme/widgets.git")
	gitT(t, "checkout", "-q", "-b", "feature/x")
	commitT(t, "first")
	return repo, data
}

func gitT(t *testing.T, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "core.hooksPath=/dev/null"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitT(t *testing.T, msg string) string {
	t.Helper()
	gitT(t, "commit", "-q", "--allow-empty", "-m", msg)
	return gitT(t, "rev-parse", "HEAD")
}

func ledgerLines(t *testing.T, data string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(data, "ledger.jsonl"))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("not JSON: %q", sc.Text())
		}
		out = append(out, m)
	}
	return out
}

func seedLedger(t *testing.T, data string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "ledger.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func eventsNamed(lines []map[string]any, name string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["event"] == name {
			out = append(out, l)
		}
	}
	return out
}

// Guards: a commit that cannot be traced to the prompt that produced it. The Agent-Session
// trailer names only the session, and one real session spanned 190 PRs; commit.recorded must
// carry the newest prompt of the committing session (not another session's) and its ticket key
// (the branch's, else the commit message's, else no-ticket), and a commit made
// while a rebase is in progress is recorded: new work at a break, edit or conflict stop looks
// to git's state exactly like a replayed pick, which post-rewrite's map marks instead.
func TestPostCommitRecordsTheCommittingPrompt(t *testing.T) {
	const (
		mine   = `{"schema_version":1,"ts":"2026-11-12T10:00:00+00:00","event":"prompt.submitted","repo":"acme/widgets","branch":"feature/x","session_id":"s-1","prompt_id":"p-old"}`
		newer  = `{"schema_version":1,"ts":"2026-11-12T10:05:00+00:00","event":"prompt.submitted","repo":"acme/widgets","branch":"feature/x","session_id":"s-1","prompt_id":"p-new"}`
		others = `{"schema_version":1,"ts":"2026-11-12T10:06:00+00:00","event":"prompt.submitted","repo":"acme/widgets","branch":"feature/x","session_id":"s-2","prompt_id":"p-other"}`
	)
	t.Run("agent commit", func(t *testing.T) {
		_, data := commitRepo(t)
		seedLedger(t, data, mine, newer, others)
		sha := commitT(t, "second")
		if code := PostCommit(nil); code != 0 {
			t.Fatalf("exit %d", code)
		}
		got := eventsNamed(ledgerLines(t, data), "commit.recorded")
		if len(got) != 1 {
			t.Fatalf("got %d commit.recorded lines, want 1: %v", len(got), ledgerLines(t, data))
		}
		want := map[string]any{"commit_sha": sha, "session_id": "s-1", "harness": "claude-code", "prompt_id": "p-new",
			"repo": "acme/widgets", "branch": "feature/x", "ticket_key": "no-ticket"}
		for k, v := range want {
			if got[0][k] != v {
				t.Errorf("commit.recorded %s = %v, want %v", k, got[0][k], v)
			}
		}
	})
	t.Run("the ticket key from the commit message when the branch has none", func(t *testing.T) {
		_, data := commitRepo(t)
		seedLedger(t, data, newer)
		commitT(t, "PROJ-7: fix the login")
		PostCommit(nil)
		if got := eventsNamed(ledgerLines(t, data), "commit.recorded"); len(got) != 1 || got[0]["ticket_key"] != "PROJ-7" {
			t.Errorf("got %v, want one commit.recorded with ticket_key PROJ-7", got)
		}
	})
	t.Run("a commit made during a rebase is recorded", func(t *testing.T) {
		_, data := commitRepo(t)
		seedLedger(t, data, newer)
		// A rebase in progress, as git leaves it while replaying a pick and at a break stop: the
		// two cannot be told apart, so the commit is recorded, and post-rewrite marks a replay.
		dir := gitT(t, "rev-parse", "--path-format=absolute", "--git-path", "rebase-merge")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		sha := commitT(t, "hand commit at a break, or a replayed pick")
		PostCommit(nil)
		if got := eventsNamed(ledgerLines(t, data), "commit.recorded"); len(got) != 1 || got[0]["commit_sha"] != sha {
			t.Errorf("got %v, want one commit.recorded for %s", got, sha)
		}
	})
}

// Guards: a commit traced to nothing after a rebase or amend gives it a new SHA. post-rewrite
// must map each old SHA to its new one; a line git did not write that way is said, not dropped.
func TestPostRewriteRecordsTheShaMap(t *testing.T) {
	_, data := commitRepo(t)
	old := gitT(t, "rev-parse", "HEAD")
	gitT(t, "commit", "-q", "--amend", "--allow-empty", "-m", "first, amended")
	amended := gitT(t, "rev-parse", "HEAD")
	stdin := pipeOf(t, old+" "+amended+"\nnot a sha line\n")
	if code := PostRewrite([]string{"amend"}, stdin); code != 0 {
		t.Fatalf("exit %d", code)
	}
	lines := ledgerLines(t, data)
	got := eventsNamed(lines, "commit.rewritten")
	if len(got) != 1 || got[0]["old_commit_sha"] != old || got[0]["commit_sha"] != amended ||
		got[0]["rewrite_kind"] != "amend" || got[0]["session_id"] != "s-1" {
		t.Errorf("got %v, want one commit.rewritten %s -> %s (amend, session s-1)", got, old, amended)
	}
	if errs := eventsNamed(lines, "ledger.error"); len(errs) != 1 || !strings.Contains(errs[0]["reason"].(string), "post-rewrite") {
		t.Errorf("got errors %v, want one ledger.error naming post-rewrite for the malformed line", errs)
	}
}

func pipeOf(t *testing.T, s string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = w.WriteString(s); _ = w.Close() }()
	t.Cleanup(func() { _ = r.Close() })
	return r
}
