package why

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xbew/livy/internal/oldnames"
)

// twoCommitRepo is a repo with two commits, the test running inside it, Livy's data folder
// under the test's temp dir and no org in scope, so nothing reaches the network.
func twoCommitRepo(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	t.Setenv("HOME", root)
	t.Setenv("LIVY_DIR", filepath.Join(root, "data"))
	for _, k := range []string{"LIVY_SCOPE_ORGS", oldnames.LivyEnv("SCOPE_ORGS"), oldnames.RecorderEnv("ORGS")} {
		if k != "" {
			t.Setenv(k, "")
		}
	}
	gitRun(t, "init", "-q", "-b", "main", repo)
	t.Chdir(repo)
	gitRun(t, "commit", "-q", "--allow-empty", "-m", "first")
	gitRun(t, "commit", "-q", "--allow-empty", "-m", "second")
}

func gitRun(t *testing.T, args ...string) {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "core.hooksPath=/dev/null"}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// Guards #142: `livy why HEAD --per-prompt` printed the usage line, because flag parsing
// stopped at the revision; people write flags last.
func TestFlagsAfterTheRevision(t *testing.T) {
	twoCommitRepo(t)
	var out, errb bytes.Buffer
	if code := Main([]string{"HEAD", "--per-prompt"}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "commit ") {
		t.Errorf("why HEAD --per-prompt = exit %d, stdout %q, stderr %q; want exit 0 and a report", code, out.String(), errb.String())
	}
}

// Guards: an A..B written the wrong way round said only that it names no commits, and the
// user had to work out that B was behind A.
func TestEmptyRangeSaysWhy(t *testing.T) {
	twoCommitRepo(t)
	for _, tc := range []struct{ rng, want string }{
		{"HEAD..HEAD~1", "did you mean HEAD~1..HEAD?"},
		{"HEAD..HEAD", "the same commit"},
	} {
		var out, errb bytes.Buffer
		if code := Main([]string{tc.rng}, &out, &errb); code != 2 || !strings.Contains(errb.String(), tc.want) {
			t.Errorf("why %s = exit %d, stderr %q; want exit 2 and %q", tc.rng, code, errb.String(), tc.want)
		}
	}
}

// Guards: a tool reading `livy why` by parsing its text, which breaks on any wording change.
// `--json` writes one document whose field names are the contract (schema livy.why/1); the
// expected shape here is written out by hand, not taken from the package's own types.
func TestWhyJSON(t *testing.T) {
	twoCommitRepo(t)
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(out))
	ledgerLines := `{"event":"prompt.submitted","ts":"2026-11-12T10:00:00+00:00","session_id":"s-1","prompt_id":"p-1"}
{"event":"commit.recorded","ts":"2026-11-12T10:10:00+00:00","session_id":"s-1","prompt_id":"p-1","repo":"o/r","branch":"main","harness":"claude-code","commit_sha":"` + sha + `"}
`
	if err := os.MkdirAll(os.Getenv("LIVY_DIR"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("LIVY_DIR"), "ledger.jsonl"), []byte(ledgerLines), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Main([]string{"--json", "HEAD"}, &stdout, &stderr)
	var got struct {
		Schema  string `json:"schema"`
		Commits []struct {
			SHA      string `json:"sha"`
			Sessions []struct {
				ID         string         `json:"session_id"`
				WindowFrom *string        `json:"window_from"`
				WindowTo   string         `json:"window_to"`
				Prompts    []string       `json:"prompt_ids"`
				Tools      map[string]int `json:"tools"`
			} `json:"sessions"`
		} `json:"commits"`
		Read struct {
			Unread []string `json:"unread"`
		} `json:"read"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil || code != 0 {
		t.Fatalf("why --json HEAD = exit %d, %v; stdout %q, stderr %q", code, err, stdout.String(), stderr.String())
	}
	if got.Schema != "livy.why/1" || len(got.Commits) != 1 || got.Commits[0].SHA != sha || len(got.Commits[0].Sessions) != 1 {
		t.Fatalf("why --json HEAD = %+v, want schema livy.why/1 and one commit %s with one session", got, sha)
	}
	s := got.Commits[0].Sessions[0]
	if s.ID != "s-1" || s.WindowFrom != nil || s.WindowTo != "2026-11-12T10:10:00Z" || len(s.Prompts) != 1 || s.Prompts[0] != "p-1" || s.Tools == nil || got.Read.Unread == nil {
		t.Errorf("session = %+v, read.unread %v; want s-1 from the session's start (null) to 10:10:00Z, prompt p-1, tools {} and unread [] not null", s, got.Read.Unread)
	}
}
