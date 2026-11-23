package install

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The git hook goes into each repo, not a machine-wide core.hooksPath: that would override
// a repo's own hooks folder. A hooks folder that is tracked inside the work tree is
// reported, not written: an untracked file holding a home path there is one `git add -A`
// from being committed for everyone.

const gitHookSignature = "installed by `livy install`"

// trailerHook is the git hook that stamps an agent's commits with attribution trailers.
const trailerHook = "prepare-commit-msg"

// commitHooks record each commit (commit.recorded) and git's old -> new SHA map after an
// amend or a rebase (commit.rewritten). Unlike the trailer hook they are always installed.
var commitHooks = []string{"post-commit", "post-rewrite"}

// ourHooks are the git hooks install writes into a repo's own hooks folder.
var ourHooks = append([]string{"post-checkout", trailerHook}, commitHooks...)

func gitHookBody(bin string) string { return hookBody("post-checkout", bin) }

// hookBody runs `livy git <name>` with git's arguments; a missing binary is a no-op.
func hookBody(name, bin string) string {
	return "#!/bin/sh\n# livy: " + name + ", " + gitHookSignature + ". Safe to delete.\n" +
		"L=" + shellQuote(bin) + "\n" +
		"[ -x \"$L\" ] && \"$L\" git " + name + " \"$@\"\nexit 0\n"
}

// isOurGitHook is a file install wrote and nobody has edited since: one of our templates line
// for line, whatever binary path it names.
func isOurGitHook(body []byte) bool {
	got := strings.Split(string(body), "\n")
	for _, name := range ourHooks {
		if sameTemplate(got, strings.Split(hookBody(name, "X"), "\n")) {
			return true
		}
	}
	return false
}

func sameTemplate(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if i == 2 {
			if !strings.HasPrefix(got[i], "L=") {
				return false
			}
			continue
		}
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func gitOut(repo string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

// hookPath is where this repo's git looks for post-checkout, and whether that folder is
// tracked inside the work tree.
func hookPath(repo string) (path string, tracked bool, err error) {
	dir, err := gitOut(repo, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if err != nil || dir == "" {
		return "", false, fmt.Errorf("not a git repository")
	}
	if hp, _ := gitOut(repo, "config", "--get", "core.hooksPath"); hp != "" {
		top, _ := gitOut(repo, "rev-parse", "--show-toplevel")
		rel, rerr := filepath.Rel(top, dir)
		if top != "" && rerr == nil && !strings.HasPrefix(rel, "..") {
			files, _ := gitOut(repo, "ls-files", "--", rel)
			tracked = files != ""
		}
	}
	return filepath.Join(dir, "post-checkout"), tracked, nil
}

// installGitHook writes post-checkout into repo, leaving a hook that is not ours alone.
func installGitHook(repo, bin string, dry bool) (path, msg string, failed bool) {
	path, tracked, err := hookPath(repo)
	if err != nil {
		return "", fmt.Sprintf("%s: %v, skipped", repo, err), false
	}
	if tracked {
		return "", fmt.Sprintf("%s: core.hooksPath is tracked in the repo; not writing into it (add a post-checkout there, or unset core.hooksPath)", repo), false
	}
	return writeHook(path, repo, "post-checkout", bin, dry)
}

// installTrailerHook writes prepare-commit-msg beside the repo's post-checkout when trailers
// are on, and removes ours when they are off. A hook that is not ours is left alone, and the
// message says the line it needs.
func installTrailerHook(repo, bin string, on, dry bool) (path, msg string, failed bool) {
	pc, tracked, err := hookPath(repo)
	if err != nil || tracked {
		return "", "", false // installGitHook or the redirect has said why
	}
	path = filepath.Join(filepath.Dir(pc), trailerHook)
	if on {
		return writeHook(path, repo, trailerHook, bin, dry)
	}
	n := removeGitHook(path, dry)
	if strings.HasPrefix(n.text, path+": not ours") {
		return "", "", false // trailers are off and that hook is the repo's own
	}
	return "", n.text, n.failed
}

// installCommitHooks writes post-commit and post-rewrite beside the repo's post-checkout.
func installCommitHooks(repo, bin string, dry bool) []hookResult {
	pc, tracked, err := hookPath(repo)
	if err != nil || tracked {
		return nil // installGitHook or the redirect has said why
	}
	out := make([]hookResult, 0, len(commitHooks))
	for _, name := range commitHooks {
		path, msg, bad := writeHook(filepath.Join(filepath.Dir(pc), name), repo, name, bin, dry)
		out = append(out, hookResult{path, msg, bad})
	}
	return out
}

// hookResult is what writing one hook did: the file it owns, what to say, and whether it failed.
type hookResult struct {
	path, msg string
	failed    bool
}

// writeHook writes our hook `name` at path unless a hook that is not ours is there.
func writeHook(path, repo, name, bin string, dry bool) (string, string, bool) {
	want := hookBody(name, bin)
	what, foreign := "git hook", "has its own post-checkout, left alone (work.started is not recorded there)"
	switch {
	case name == trailerHook:
		what = "trailer hook (" + trailerHook + ")"
		foreign = fmt.Sprintf("has its own %s, left alone (agent commits are not stamped there; add `%s git %s \"$@\"` to it)",
			trailerHook, shellQuote(bin), trailerHook)
	case slices.Contains(commitHooks, name):
		what = "commit hook (" + name + ")"
		foreign = fmt.Sprintf("has its own %s, left alone (commits are not traced there; add `%s git %s \"$@\"` to it)",
			name, shellQuote(bin), name)
		if name == "post-rewrite" {
			foreign = strings.TrimSuffix(foreign, ")") + ", fed the same stdin git gave the hook)"
		}
	}
	if _, err := os.Lstat(path); err == nil {
		body, err := os.ReadFile(path)
		switch {
		case err != nil:
			return "", fmt.Sprintf("%s: %s unreadable (%v), left alone", repo, name, err), true
		case string(body) == want:
			return path, repo + ": " + what + " already installed", false
		case !isOurGitHook(body):
			return "", repo + ": " + foreign, false
		}
	}
	if dry {
		return path, "would write " + path, false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return "", fmt.Sprintf("%s: could not write %s: %v", repo, path, err), true
	}
	if err := os.WriteFile(path, []byte(want), 0o755); err != nil {
		return "", fmt.Sprintf("%s: could not write %s: %v", repo, path, err), true
	}
	_ = os.Chmod(path, 0o755)
	return path, repo + ": " + what + " installed", false
}

// removeGitHook deletes path only when it is ours.
func removeGitHook(path string, dry bool) note {
	body, err := os.ReadFile(path)
	if err != nil {
		return note{}
	}
	if !isOurGitHook(body) {
		return note{text: path + ": not ours (or edited since), left alone"}
	}
	if dry {
		return note{text: "would remove " + path}
	}
	if err := os.Remove(path); err != nil {
		return note{text: fmt.Sprintf("could not remove %s: %v", path, err), failed: true}
	}
	return note{text: "removed " + path}
}

// transcriptRepos are the working folders Claude Code sessions ran in, read from the
// first lines of each local transcript (only the cwd field is kept): the repos actually
// used with the agent. Callers keep only those in scope.
func transcriptRepos(claudeDir string) []string {
	seen, asked := map[string]bool{}, map[string]bool{}
	var out []string
	files, _ := filepath.Glob(filepath.Join(claudeDir, "projects", "*", "*.jsonl"))
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		for n := 0; n < 20 && sc.Scan(); n++ {
			var rec struct {
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Cwd != "" {
				if asked[rec.Cwd] {
					break
				}
				asked[rec.Cwd] = true
				top, err := gitOut(rec.Cwd, "rev-parse", "--show-toplevel")
				if err == nil && top != "" && !seen[top] {
					seen[top] = true
					out = append(out, top)
				}
				break
			}
		}
		fh.Close()
	}
	return out
}

// callsTrailerHook is a hook that runs `livy git prepare-commit-msg`: a tracked-hooks wrapper,
// or another tool's prepare-commit-msg with the line install's message tells you to add.
// A commented-out line does not count: it reads as wired and stamps nothing.
func callsTrailerHook(body []byte) bool {
	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#") && strings.Contains(line, " git "+trailerHook+" ") {
			return true
		}
	}
	return false
}

// ForeignTrailerHook says whether hooksDir holds another tool's prepare-commit-msg that does
// not call Livy: `livy install` leaves such a hook alone, so the fix is a line added to it.
func ForeignTrailerHook(hooksDir string) bool {
	body, err := os.ReadFile(filepath.Join(hooksDir, trailerHook))
	return err == nil && !isOurGitHook(body) && !callsTrailerHook(body)
}

// TrailerCoverage is, over the repos this install put Livy's git hooks in, how many there
// are and which of them do not stamp agent commits: a prepare-commit-msg beside post-checkout
// that is neither ours nor calls Livy, or a tracked-hooks wrapper that does not call Livy.
func TrailerCoverage(st State) (repos int, missing []string) {
	var dirs []string
	for _, h := range st.GitHooks {
		if _, err := os.Stat(h); err == nil && !slices.Contains(dirs, filepath.Dir(h)) {
			dirs = append(dirs, filepath.Dir(h)) // a hook that is gone: doctor's other rows say so
		}
	}
	for _, d := range dirs {
		repos++
		if body, err := os.ReadFile(filepath.Join(d, trailerHook)); err != nil || (!isOurGitHook(body) && !callsTrailerHook(body)) {
			missing = append(missing, d)
		}
	}
	for _, r := range st.HookRedirects {
		if _, err := os.Stat(r.GitDir); err != nil {
			continue
		}
		repos++
		body, err := os.ReadFile(filepath.Join(r.Dir, trailerHook))
		if err != nil || !callsTrailerHook(body) {
			missing = append(missing, r.GitDir)
		}
	}
	return repos, missing
}

// HookGaps is, over the repos this install put Livy's git hooks in, the hooks folders where a
// hook it wrote has since gone, and the repos that are gone altogether. A plain `livy install`
// puts back the first; the second has nothing to put back.
func HookGaps(st State) (missing, gone []string) {
	for _, h := range st.GitHooks {
		dir := filepath.Dir(h)
		if _, err := os.Stat(RepoOfHooks(dir)); errors.Is(err, fs.ErrNotExist) {
			gone = appendNew(gone, RepoOfHooks(dir))
		} else if _, err := os.Lstat(h); errors.Is(err, fs.ErrNotExist) {
			// Only a hook that is not there is missing; one we can't inspect is not reported as gone.
			missing = appendNew(missing, dir)
		}
	}
	return missing, gone
}
