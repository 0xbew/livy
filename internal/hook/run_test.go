//go:build unix

package hook

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// orgRepo makes a git repo whose origin is under the org the environment puts in scope,
// and runs the test from inside it with the data dir under the test's temp dir.
func orgRepo(t *testing.T) (data string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	for _, args := range [][]string{{"init", "-q", "-b", "main", repo},
		{"-C", repo, "remote", "add", "origin", "git@github.com:acme/widgets.git"},
		{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "--allow-empty", "-m", "i"},
		{"-C", repo, "checkout", "-q", "-b", "feature/x"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Chdir(repo)
	data = filepath.Join(root, "data")
	t.Setenv("HOME", root) // hermetic: the old-name notes look at the old user folder
	t.Setenv("LIVY_DIR", data)
	t.Setenv("LIVY_SCOPE_ORGS", "acme")
	t.Setenv("LIVY_GIT_TIMEOUT", "") // a developer's own value would decide every git call
	os.Unsetenv("LIVY_GIT_TIMEOUT")
	t.Setenv("LIVY_CONFORMANCE_NOW", "1793707200")
	return data
}

func stdinWith(t *testing.T, payload string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.WriteString(w, payload); w.Close() }()
	t.Cleanup(func() { r.Close() })
	return r
}

func lines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
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

// The hard rules as properties: every failure exits 0, and a line that cannot be written
// as asked leaves a ledger.error saying why. Expected values are the Python ledger's
// documented behaviour (ledger.py run()).
func TestEveryFailureExitsZeroAndIsSaid(t *testing.T) {
	// Guards: a hook that blocks the tool loop, and a broken hook that looks like a quiet one.
	t.Run("malformed stdin records the error and the event", func(t *testing.T) {
		data := orgRepo(t)
		if code := Run([]string{"turn.ended"}, stdinWith(t, `{"session_id": "abc", `), io.Discard); code != 0 {
			t.Fatalf("exit %d", code)
		}
		got := lines(t, filepath.Join(data, "ledger.jsonl"))
		if len(got) != 2 || got[0]["event"] != "ledger.error" || got[1]["event"] != "turn.ended" {
			t.Fatalf("got %v, want a ledger.error then turn.ended", got)
		}
		if got[0]["reason"] != "stdin was not valid JSON (event turn.ended still recorded)" ||
			got[0]["repo"] != "acme/widgets" || got[1]["branch"] != "feature/x" || got[1]["raw_stdin_bytes"] != 22.0 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("unknown event is an error and nothing else", func(t *testing.T) {
		data := orgRepo(t)
		if code := Run([]string{"not.a.real.event"}, stdinWith(t, ""), io.Discard); code != 0 {
			t.Fatalf("exit %d", code)
		}
		got := lines(t, filepath.Join(data, "ledger.jsonl"))
		if len(got) != 1 || got[0]["reason"] != "unknown event name: 'not.a.real.event'" {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("data dir that cannot be made still exits 0", func(t *testing.T) {
		data := orgRepo(t)
		if err := os.WriteFile(data, []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code := Run([]string{"turn.ended"}, stdinWith(t, "{}"), io.Discard); code != 0 {
			t.Fatalf("exit %d", code)
		}
	})
	t.Run("an event after a torn last line starts its own line", func(t *testing.T) {
		// Guards: a hook killed mid-write leaves a fragment, and the next event glued to it is lost with it.
		data := orgRepo(t)
		if err := os.MkdirAll(data, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(data, "ledger.jsonl")
		if err := os.WriteFile(path, []byte(`{"event":"turn.st`), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := Run([]string{"turn.ended"}, stdinWith(t, "{}"), io.Discard); code != 0 {
			t.Fatalf("exit %d", code)
		}
		raw, _ := os.ReadFile(path)
		rows := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		var last map[string]any
		if err := json.Unmarshal([]byte(rows[len(rows)-1]), &last); err != nil || last["event"] != "turn.ended" {
			t.Fatalf("after a torn line the ledger is:\n%s\nwant turn.ended on a line of its own", raw)
		}
	})
	t.Run("a ledger the torn check cannot read still gets the event, and a ledger.error", func(t *testing.T) {
		// Guards: the torn check costing an event, or skipped with nothing said.
		data := orgRepo(t)
		if err := os.MkdirAll(data, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(data, "ledger.jsonl")
		if err := os.WriteFile(path, nil, 0o200); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
		if f, err := os.Open(path); err == nil {
			_ = f.Close()
			t.Skip("a write-only file is readable here (root)")
		}
		if code := Run([]string{"turn.ended"}, stdinWith(t, "{}"), io.Discard); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		got := lines(t, path)
		events := map[any]int{}
		for _, l := range got {
			events[l["event"]]++
		}
		if events["turn.ended"] != 1 || events["ledger.error"] != 1 {
			t.Fatalf("got %v, want turn.ended and one ledger.error saying the torn check did not run", got)
		}
	})
	t.Run("out of scope leaves no trace", func(t *testing.T) {
		data := orgRepo(t)
		t.Setenv("LIVY_SCOPE_ORGS", "someone-else")
		Run([]string{"session.started"}, stdinWith(t, `{"session_id":"s"}`), io.Discard)
		if _, err := os.Stat(data); !os.IsNotExist(err) {
			t.Errorf("data dir exists (err %v)", err)
		}
	})
}

// Guards: a repository named in scope.repos that records nothing because its owner is not an
// in-scope org, and a repo entry that brings in its owner's other repositories.
func TestScopeReposRecordsTheListedRepoAndNoneOfItsSiblings(t *testing.T) {
	for _, c := range []struct {
		repos    string
		recorded bool
	}{
		{"Acme/Widgets", true},
		{"acme/widgets-old", false},
		{"acme", false},
	} {
		t.Run(c.repos, func(t *testing.T) {
			data := orgRepo(t)
			t.Setenv("LIVY_SCOPE_ORGS", "")
			t.Setenv("LIVY_SCOPE_REPOS", c.repos)
			Run([]string{"session.started"}, stdinWith(t, `{"session_id":"s"}`), io.Discard)
			got := lines(t, filepath.Join(data, "ledger.jsonl"))
			if c.recorded && (len(got) != 1 || got[0]["event"] != "session.started" || got[0]["repo"] != "acme/widgets") {
				t.Errorf("scope.repos %q: got %v, want one session.started for acme/widgets", c.repos, got)
			}
			if _, err := os.Stat(data); !c.recorded && !os.IsNotExist(err) {
				t.Errorf("scope.repos %q: data dir exists (err %v), want no trace", c.repos, err)
			}
		})
	}
}

// No network in the tool loop, checked where it cannot be bypassed: the hook's import
// graph holds no networking package.
func TestHookImportsNoNetwork(t *testing.T) {
	// Guards: a network call added to the hook path, which would stall Claude Code's tool loop.
	out, err := exec.Command("go", "list", "-deps", "github.com/0xbew/livy/internal/hook").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == "net" || strings.HasPrefix(dep, "net/") || dep == "crypto/tls" {
			t.Errorf("the hook imports %s", dep)
		}
	}
}

// Guards: a git timeout while resolving a work.pr_opened's target folder dropping the
// target fields with nothing said (Python does exactly that: tool_target reads the timed-out
// origin as "no repo").
func TestATargetLookupTimeoutIsSaid(t *testing.T) {
	data := orgRepo(t)
	other := filepath.Join(filepath.Dir(data), "other")
	for _, args := range [][]string{{"init", "-q", "-b", "main", other},
		{"-C", other, "remote", "add", "origin", "git@github.com:acme/other.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// A git that hangs only when asked for the other checkout's origin.
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *\"" + other + " remote get-url\"*) sleep 5;; esac\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("LIVY_GIT_TIMEOUT", "0.2")
	payload := `{"tool_name":"Bash","tool_input":{"command":"cd ` + other + ` && gh pr create --fill"}}`
	if code := Run([]string{"work.pr_opened"}, stdinWith(t, payload), io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := lines(t, filepath.Join(data, "ledger.jsonl"))
	var said, event bool
	for _, l := range got {
		reason, _ := l["reason"].(string)
		if l["event"] == "ledger.error" && strings.Contains(reason, "timed out") && strings.Contains(reason, "target") {
			said = true
		}
		if l["event"] == "work.pr_opened" {
			event = l["target_via"] == "cd"
		}
	}
	if !said || !event {
		t.Errorf("got %v, want work.pr_opened (target_via cd) and a ledger.error naming the target timeout", got)
	}
}

// Guards: a git timeout reading the branch writing branch "unknown" with nothing said, so a
// slow git reads exactly like a folder outside a repo and the event's tokens and prompts
// join to no PR.
func TestABranchLookupTimeoutIsSaid(t *testing.T) {
	data := orgRepo(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *--abbrev-ref*) sleep 5;; esac\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("LIVY_GIT_TIMEOUT", "0.2")
	if code := Run([]string{"turn.ended"}, stdinWith(t, `{"session_id":"s"}`), io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var said, event bool
	for _, l := range lines(t, filepath.Join(data, "ledger.jsonl")) {
		reason, _ := l["reason"].(string)
		said = said || l["event"] == "ledger.error" && strings.Contains(reason, "timed out") && strings.Contains(reason, "branch")
		event = event || l["event"] == "turn.ended" && l["branch"] == "unknown"
	}
	if !said || !event {
		t.Errorf("want turn.ended with branch unknown and a ledger.error naming the branch timeout; said=%v event=%v", said, event)
	}
}

// Guards: a work boundary that cannot be grouped by ticket. work.started carries the key
// derived from the branch, and the explicit no-ticket key, never an absent one, when the
// branch names none.
func TestWorkStartedCarriesTheTicketKey(t *testing.T) {
	for _, c := range []struct{ branch, want string }{
		{"feature/PROJ-123-add-login", "PROJ-123"},
		{"telemetry/followup", "no-ticket"},
	} {
		t.Run(c.branch, func(t *testing.T) {
			data := orgRepo(t)
			if out, err := exec.Command("git", "checkout", "-q", "-b", c.branch).CombinedOutput(); err != nil {
				t.Fatalf("git checkout: %v %s", err, out)
			}
			Run([]string{"work.started", "hook=post-checkout"}, stdinWith(t, ""), io.Discard)
			got := lines(t, filepath.Join(data, "ledger.jsonl"))
			if len(got) != 1 || got[0]["event"] != "work.started" || got[0]["ticket_key"] != c.want {
				t.Errorf("got %v, want one work.started with ticket_key %q", got, c.want)
			}
		})
	}
}
