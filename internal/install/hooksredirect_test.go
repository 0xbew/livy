package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Guards: routing a repo's tracked core.hooksPath through Livy changing what the repo's own
// hooks do (a blocking commit-msg must still block, with its exit code) or uninstall leaving
// the repo pointing at a deleted folder instead of its original hooks folder.
func TestTrackedHooksPathStillBlocksAndIsRestored(t *testing.T) {
	git := func(dir string, args ...string) string {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, _ := c.CombinedOutput()
		return string(out)
	}
	repo, home := t.TempDir(), t.TempDir()
	git(repo, "init", "-q")
	if err := os.MkdirAll(filepath.Join(repo, ".githooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".githooks", "commit-msg"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(repo, "config", "--local", "core.hooksPath", ".githooks")
	git(repo, "add", ".githooks")
	rec, msg, bad := installRedirect(repo, hookCmd{bin: "/nonexistent/livy", trailers: true}, home, nil, false)
	if bad || rec == nil {
		t.Fatalf("install: %s", msg)
	}
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repo, "add", "f")
	git(repo, "commit", "-q", "-m", "x")
	if out := git(repo, "rev-parse", "-q", "--verify", "HEAD"); out != "" {
		t.Errorf("the tracked commit-msg should still block the commit, HEAD is %q", out)
	}
	if m, bad := removeRedirect(*rec, false); bad {
		t.Fatal(m)
	}
	if got := git(repo, "config", "--local", "core.hooksPath"); got != ".githooks\n" {
		t.Errorf("core.hooksPath after uninstall = %q, want .githooks", got)
	}
}

func gitIn(dir string, args ...string) string {
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, _ := c.CombinedOutput()
	return string(out)
}

func trackedRepo(t *testing.T, hooks map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	gitIn(repo, "init", "-q")
	if err := os.MkdirAll(filepath.Join(repo, ".githooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range hooks {
		if err := os.WriteFile(filepath.Join(repo, ".githooks", name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(repo, "config", "--local", "core.hooksPath", ".githooks")
	gitIn(repo, "add", ".githooks")
	return repo
}

// Guards: redirecting core.hooksPath silently disabling a tracked hook Livy has no wrapper for
// (post-index-change, reference-transaction), including one the repo adds after install.
func TestEveryTrackedHookStillRunsIncludingOneAddedLater(t *testing.T) {
	marker := func(name string) string { return "#!/bin/sh\ntouch \"$(git rev-parse --git-dir)/ran-" + name + "\"\n" }
	repo := trackedRepo(t, map[string]string{"post-index-change": marker("post-index-change")})
	if _, msg, bad := installRedirect(repo, hookCmd{bin: "/nonexistent/livy", trailers: true}, t.TempDir(), nil, false); bad {
		t.Fatal(msg)
	}
	_ = os.Remove(filepath.Join(repo, ".git", "ran-post-index-change")) // the setup's own git add ran it
	if err := os.WriteFile(filepath.Join(repo, ".githooks", "reference-transaction"), []byte(marker("reference-transaction")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(repo, "add", "f")
	gitIn(repo, "commit", "-q", "-m", "x")
	for _, name := range []string{"post-index-change", "reference-transaction"} {
		if _, err := os.Stat(filepath.Join(repo, ".git", "ran-"+name)); err != nil {
			t.Errorf("tracked %s did not run after the redirect", name)
		}
	}
}

// Guards: a redirect recorded under a linked worktree's path that is later deleted, leaving
// uninstall unable to restore core.hooksPath for the repo's surviving checkout.
func TestRedirectInstalledFromAWorktreeIsRestoredAfterItIsRemoved(t *testing.T) {
	repo := trackedRepo(t, map[string]string{"commit-msg": "#!/bin/sh\nexit 0\n"})
	gitIn(repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "base")
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(repo, "worktree", "add", "-q", "-b", "w", wt)
	rec, msg, bad := installRedirect(wt, hookCmd{bin: "/nonexistent/livy", trailers: true}, t.TempDir(), nil, false)
	if bad || rec == nil {
		t.Fatalf("install: %s", msg)
	}
	gitIn(repo, "worktree", "remove", "--force", wt)
	if m, bad := removeRedirect(*rec, false); bad {
		t.Fatal(m)
	}
	if got := gitIn(repo, "config", "--local", "core.hooksPath"); got != ".githooks\n" {
		t.Errorf("core.hooksPath after uninstall = %q, want .githooks", got)
	}
}

// Guards: a redirect that stops covering a repo with nothing on the machine saying so: a
// worktree-scope core.hooksPath (extensions.worktreeConfig) outranks the local one Livy set,
// and a server-side hook the tracked folder gains after install has no Livy wrapper, so
// git runs neither the wrapper nor, once redirected, the tracked hook.
func TestRedirectProblems(t *testing.T) {
	for _, c := range []struct {
		name   string
		hooks  map[string]string // the tracked folder at install; commit-msg when nil
		change func(t *testing.T, repo string)
		want   string // "" = no problem
	}{
		{"untouched", nil, func(*testing.T, string) {}, ""},
		// a permanent false alarm on a repo whose server-side hook install already wrapped
		{"push-to-checkout wrapped at install", map[string]string{"push-to-checkout": "#!/bin/sh\nexit 0\n"}, func(*testing.T, string) {}, ""},
		{"worktree-scope hooksPath", nil, func(t *testing.T, repo string) {
			gitIn(repo, "config", "--local", "extensions.worktreeConfig", "true")
			gitIn(repo, "config", "--worktree", "core.hooksPath", "/elsewhere")
		}, "worktree scope"},
		{"worktree-scope hooksPath, worktreeConfig spelled on", nil, func(t *testing.T, repo string) {
			gitIn(repo, "config", "--local", "extensions.worktreeConfig", "on")
			gitIn(repo, "config", "--worktree", "core.hooksPath", "/elsewhere")
		}, "worktree scope"},
		// the hooksPath lives in a file config.worktree includes, which a plain --file read skips
		{"worktree-scope hooksPath set through an include", nil, func(t *testing.T, repo string) {
			gitIn(repo, "config", "--local", "extensions.worktreeConfig", "true")
			extra := filepath.Join(t.TempDir(), "extra.gitconfig")
			if err := os.WriteFile(extra, []byte("[core]\n\thooksPath = /elsewhere\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(repo, "config", "--worktree", "include.path", extra)
		}, "worktree scope"},
		// A repo's setup step (`git config core.hooksPath .githooks`) run in a linked worktree
		// writes the shared local config: every checkout's commits bypassed Livy for 2.7 days.
		{"local hooksPath set back from a worktree", nil, func(t *testing.T, repo string) {
			wt := filepath.Join(t.TempDir(), "wt")
			gitIn(repo, "worktree", "add", "-q", wt)
			gitIn(wt, "config", "core.hooksPath", ".githooks")
		}, "no longer names"},
		{"worktreeConfig on but no worktree hooksPath", nil, func(t *testing.T, repo string) {
			gitIn(repo, "config", "--local", "extensions.worktreeConfig", "true")
		}, ""},
		{"push-to-checkout added after install", nil, func(t *testing.T, repo string) {
			if err := os.WriteFile(filepath.Join(repo, ".githooks", "push-to-checkout"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "push-to-checkout"},
		{"proc-receive added after install", nil, func(t *testing.T, repo string) {
			if err := os.WriteFile(filepath.Join(repo, ".githooks", "proc-receive"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "proc-receive"},
	} {
		t.Run(c.name, func(t *testing.T) {
			hooks := c.hooks
			if hooks == nil {
				hooks = map[string]string{"commit-msg": "#!/bin/sh\nexit 0\n"}
			}
			repo := trackedRepo(t, hooks)
			home := t.TempDir()
			rec, msg, bad := installRedirect(repo, hookCmd{bin: "/nonexistent/livy", trailers: true}, home, nil, false)
			if bad || rec == nil {
				t.Fatalf("install: %s", msg)
			}
			c.change(t, repo)
			got := strings.Join(RedirectProblems(State{HookRedirects: []hookRedirect{*rec}}), "; ")
			if (c.want == "") != (got == "") || !strings.Contains(got, c.want) || (got != "" && !strings.Contains(got, "livy install")) {
				t.Errorf("problems %q; want one naming %q and `livy install`", got, c.want)
			}
		})
	}
}
