package install

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A repo whose core.hooksPath is a folder tracked in the work tree (.githooks, husky) is the
// repo's, so Livy never writes into it. Instead the repo's LOCAL core.hooksPath points at a
// Livy-owned folder of wrappers: each runs the tracked hook with the same arguments, stdin
// and exit code, and post-checkout then also runs Livy's. Uninstall puts the setting back.

// hookRedirect is what install changed in one repo, so uninstall can undo exactly that. It is
// keyed by the repo's common git dir, which every linked worktree shares and which outlives them.
type hookRedirect struct {
	// Dir is the Livy-owned hooks folder the repo's local core.hooksPath now names.
	Dir string `json:"dir"`
	// Original is the local core.hooksPath value before; HadOriginal false means it was unset
	// locally (the tracked folder came from another config scope).
	Original    string `json:"original"`
	HadOriginal bool   `json:"had_original"`
	// Tracked is the hooks folder the wrappers run, as the repo named it.
	Tracked string `json:"tracked"`
	GitDir  string `json:"git_dir"`
	// Work is a checkout of the repo that existed at install, for messages only.
	Work string `json:"work,omitempty"`
}

// notWrapped are the hooks whose absence means something other than "nothing to run": git
// reads their output (fsmonitor-watchman) or replaces its own behaviour (push-to-checkout,
// proc-receive, the p4 hooks). They get a wrapper only when the tracked folder holds one.
var notWrapped = map[string]bool{"fsmonitor-watchman": true, "push-to-checkout": true, "proc-receive": true,
	"p4-changelist": true, "p4-prepare-changelist": true, "p4-post-changelist": true, "p4-pre-submit": true}

// standardHooks are every hook git runs on a client; each gets a wrapper that runs the tracked
// hook when the repo has one at that moment, so a hook added later still runs.
var standardHooks = []string{"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit", "pre-merge-commit",
	"prepare-commit-msg", "commit-msg", "post-commit", "pre-rebase", "post-checkout", "post-merge", "pre-push",
	"pre-auto-gc", "post-rewrite", "reference-transaction", "post-index-change", "sendemail-validate",
	"pre-receive", "update", "post-receive", "post-update"}

// wrapperNames are the standard hooks (but the ones in notWrapped) plus every executable file the
// tracked folder holds now.
func wrapperNames(trackedAbs string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range standardHooks {
		if !notWrapped[n] && !seen[n] {
			seen[n], out = true, append(out, n)
		}
	}
	ents, _ := os.ReadDir(trackedAbs)
	for _, e := range ents {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 && !seen[e.Name()] {
			seen[e.Name()], out = true, append(out, e.Name())
		}
	}
	return out
}

// hookCmd is the Livy binary the wrappers call, and whether prepare-commit-msg calls it too.
type hookCmd struct {
	bin      string
	trailers bool
}

// chains is whether the wrapper for name runs Livy's hook after the tracked one.
func (c hookCmd) chains(name string) bool {
	return name == "post-checkout" || slices.Contains(commitHooks, name) || (name == trailerHook && c.trailers)
}

// readsStdin are the chained hooks git feeds a list on stdin, which the tracked hook and
// Livy's must both get.
var readsStdin = map[string]bool{"post-rewrite": true}

// wrapperBody runs the repo's tracked hook (found under the working tree git runs hooks in,
// so a linked worktree uses its own checkout of it), keeping its stdin and exit code.
func wrapperBody(name, trackedRel string, c hookCmd) string {
	b := "#!/bin/sh\n# livy: wraps the repo's own " + name + " hook, " + gitHookSignature + ". Safe to delete.\n" +
		"T=" + shellQuote(trackedRel) + "\n" +
		"case \"$T\" in /*) ;; *) T=\"$PWD/$T\" ;; esac\n" +
		"T=\"$T/" + name + "\"\n"
	if !c.chains(name) {
		return b + "[ -x \"$T\" ] && exec \"$T\" \"$@\"\nexit 0\n"
	}
	// The tracked hook's code is the hook's code; a failure in Livy's part cannot change it.
	if readsStdin[name] {
		// The x keeps the list's trailing newline, which $(...) would strip.
		return b + "in=$(cat; echo x); in=${in%x}\nrc=0\n" +
			"[ -x \"$T\" ] && { printf '%s' \"$in\" | \"$T\" \"$@\"; rc=$?; }\n" +
			"L=" + shellQuote(c.bin) + "\n[ -x \"$L\" ] && printf '%s' \"$in\" | \"$L\" git " + name + " \"$@\" >/dev/null 2>&1\nexit $rc\n"
	}
	return b + "rc=0\n[ -x \"$T\" ] && { \"$T\" \"$@\"; rc=$?; }\n" +
		"L=" + shellQuote(c.bin) + "\n[ -x \"$L\" ] && \"$L\" git " + name + " \"$@\" >/dev/null 2>&1\nexit $rc\n"
}

func redirectDir(home, repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return filepath.Join(home, ".livy", "githooks", hex.EncodeToString(sum[:])[:12])
}

// gitDirOut runs git against a git dir directly, so it works after every checkout of the repo
// is gone.
func gitDirOut(gitDir string, args ...string) (string, error) {
	return gitOut(gitDir, append([]string{"--git-dir=" + gitDir}, args...)...)
}

func localHooksPath(gitDir string) (string, bool) {
	v, err := gitDirOut(gitDir, "config", "--local", "--get", "core.hooksPath")
	return v, err == nil
}

func commonDir(repo string) string {
	d, err := gitOut(repo, "rev-parse", "--git-common-dir") // --path-format needs git 2.31
	if err != nil || d == "" {
		return ""
	}
	if !filepath.IsAbs(d) {
		d = filepath.Join(repo, d)
	}
	return filepath.Clean(d)
}

// installRedirect points repo's local core.hooksPath at a Livy folder wrapping the tracked
// one. prev is this repo's record from an earlier install, which a re-install refreshes.
func installRedirect(repo string, c hookCmd, home string, prev *hookRedirect, dry bool) (*hookRedirect, string, bool) {
	gd := commonDir(repo)
	if gd == "" {
		return nil, repo + ": not a git repository, skipped", false
	}
	dir := redirectDir(home, gd)
	rec := &hookRedirect{GitDir: gd, Dir: dir, Work: repo}
	cur, had := localHooksPath(gd)
	if prev != nil && cur == dir {
		*rec = *prev
		rec.Work = repo
	} else {
		rec.Original, rec.HadOriginal = cur, had
		rec.Tracked, _ = gitOut(repo, "config", "--type=path", "--get", "core.hooksPath")
	}
	trackedRel := rec.Tracked
	if trackedRel == "" {
		return nil, repo + ": core.hooksPath is empty, left alone", true
	}
	if dry {
		return rec, fmt.Sprintf("%s: would route core.hooksPath (%s) through %s", repo, trackedRel, dir), false
	}
	trackedAbs := trackedRel
	if !filepath.IsAbs(trackedAbs) {
		top, _ := gitOut(repo, "rev-parse", "--show-toplevel")
		trackedAbs = filepath.Join(top, trackedRel)
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, fmt.Sprintf("%s: could not write %s: %v", repo, dir, err), true
	}
	for _, name := range wrapperNames(trackedAbs) {
		if err := writeFileAtomic(filepath.Join(dir, name), []byte(wrapperBody(name, trackedRel, c)), 0o755); err != nil {
			return nil, fmt.Sprintf("%s: could not write %s: %v", repo, filepath.Join(dir, name), err), true
		}
	}
	if cur != dir {
		if _, err := gitDirOut(gd, "config", "--local", "core.hooksPath", dir); err != nil {
			return nil, fmt.Sprintf("%s: could not set core.hooksPath: %v", repo, err), true
		}
	}
	return rec, fmt.Sprintf("%s: tracked hooks kept; core.hooksPath routed through %s", repo, dir), false
}

// removeRedirect restores the repo's local core.hooksPath and deletes the Livy folder. A
// setting changed since install is left as it is.
func removeRedirect(r hookRedirect, dry bool) (string, bool) {
	cur, _ := localHooksPath(r.GitDir)
	var msgs []string
	switch {
	case cur != r.Dir:
		msgs = append(msgs, r.GitDir+": core.hooksPath changed since install, left alone")
	case dry:
		msgs = append(msgs, r.GitDir+": would restore core.hooksPath")
	default:
		var err error
		if r.HadOriginal {
			_, err = gitDirOut(r.GitDir, "config", "--local", "core.hooksPath", r.Original)
		} else {
			_, err = gitDirOut(r.GitDir, "config", "--local", "--unset", "core.hooksPath")
		}
		if err != nil {
			return fmt.Sprintf("could not restore core.hooksPath in %s: %v", r.GitDir, err), true
		}
		msgs = append(msgs, r.GitDir+": core.hooksPath restored")
	}
	if !dry && cur == r.Dir {
		_ = os.RemoveAll(r.Dir)
	}
	return strings.Join(msgs, "; "), false
}

// routedThroughLivy is whether the repo's local core.hooksPath already names one of Livy's
// wrapper folders although no record says so (a lost state file).
func routedThroughLivy(repo, home string) bool {
	cur, ok := localHooksPath(commonDir(repo))
	return ok && filepath.Dir(cur) == filepath.Join(home, ".livy", "githooks")
}

// RedirectProblems is what has stopped one of the recorded redirects covering its repo since
// install, one line each: the local core.hooksPath no longer names Livy's folder (a repo's own
// setup step sets it back, from any checkout), a worktree-scope core.hooksPath outranks it, or
// a server-side hook the tracked folder gained later has no wrapper.
func RedirectProblems(st State) []string {
	var out []string
	for _, r := range st.HookRedirects {
		if _, err := os.Stat(r.GitDir); err != nil {
			continue // the repo is gone; uninstall tidies the record
		}
		if p := replacedRedirect(r); p != "" {
			out = append(out, p)
			continue // the other checks are about a redirect that is still in place
		}
		out = append(out, worktreeOverrides(r)...)
		out = append(out, unwrappedHooks(r)...)
	}
	return out
}

// replacedRedirect is a problem when the repo's local core.hooksPath no longer names Livy's
// folder, "" while it does.
func replacedRedirect(r hookRedirect) string {
	cur, ok := localHooksPath(r.GitDir)
	if ok && cur == r.Dir {
		return ""
	}
	if !ok {
		cur = "unset"
	}
	return fmt.Sprintf("%s: core.hooksPath (%s) no longer names Livy's %s, so no commit in any checkout of the repo, worktrees included, is recorded; re-run `livy install`", r.GitDir, cur, r.Dir)
}

// worktreeOverrides is a problem for each worktree-scope core.hooksPath that outranks the
// one Livy set.
func worktreeOverrides(r hookRedirect) []string {
	var out []string
	if v, _ := gitDirOut(r.GitDir, "config", "--local", "--bool", "--get", "extensions.worktreeConfig"); v != "true" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(r.GitDir, "worktrees", "*", "config.worktree"))
	for _, f := range append(files, filepath.Join(r.GitDir, "config.worktree")) {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		if v, err := gitOut(r.GitDir, "config", "--file", f, "--includes", "--get", "core.hooksPath"); err == nil && v != r.Dir {
			out = append(out, fmt.Sprintf("%s: core.hooksPath is also set in worktree scope (%s, in %s) and outranks the one Livy set, so the repo's hooks bypass Livy's wrappers; clear it, then re-run `livy install`", r.GitDir, v, f))
		}
	}
	return out
}

// unwrappedHooks is a problem for each hook the tracked hooks folder gained after install
// that Livy has no wrapper for.
func unwrappedHooks(r hookRedirect) []string {
	var out []string
	tracked := r.Tracked
	if !filepath.IsAbs(tracked) {
		top := filepath.Dir(r.GitDir)
		if r.Work != "" {
			if t, err := gitOut(r.Work, "rev-parse", "--show-toplevel"); err == nil && t != "" {
				top = t
			}
		}
		tracked = filepath.Join(top, tracked)
	}
	for _, name := range sortedKeys(notWrapped) {
		if fi, err := os.Stat(filepath.Join(tracked, name)); err != nil || fi.IsDir() {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(r.Dir, name)); err != nil || !strings.Contains(string(b), gitHookSignature) {
			out = append(out, fmt.Sprintf("%s: the tracked hooks folder gained %s after install and Livy has no wrapper for it, so it no longer runs; re-run `livy install`", r.GitDir, name))
		}
	}
	return out
}
