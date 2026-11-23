package prompts

import (
	"flag"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/extract"
	"github.com/0xbew/livy/internal/extract/claudecode"
	"github.com/0xbew/livy/internal/extract/codex"
	"github.com/0xbew/livy/internal/gitinfo"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/vault"
)

// defaultBackfillDays is how far back `livy vault backfill` reaches unless --days says: the
// vault's whole retention.
const defaultBackfillDays = vault.KeepDays

// VaultBackfill copies into the vault the text of every in-scope prompt sent in the last days
// that the transcripts on disk still hold: those sent in an in-scope repo, and every prompt of
// a session that committed to one from a folder outside it. It writes only the vault and its ledger.errors, and
// returns its counts. A prompt already kept is not written again.
func VaultBackfill(o Options, days int) map[string]any {
	w := &extract.Writer{Dir: o.Dir, Stderr: o.Stderr}
	scope := extract.NewScope(o.Stderr)
	scope.Git.DefaultTimeout = backfillGitTimeout // off the hook path: wait longer than a hook may
	since := o.Now.Add(-time.Duration(days) * 24 * time.Hour)
	files, _ := listTranscripts(o, scope)
	counts := map[string]any{"files": len(files)}
	var found, outsideRepos []claudecode.Prompt
	seen := map[string]bool{}
	unreadable, held, outside, noTime, noSession := 0, 0, 0, 0, 0
	for _, f := range files {
		before := scope.TimeoutReads
		read, err := f.read(0, 0)
		if err != nil {
			unreadable++
			continue
		}
		unreadable += read.Bad
		noSession += len(read.NoSession)
		if read.Held != "" {
			held++
			w.Error("prompt_vault: backfill: a transcript holds " + read.Held + " — the transcript format may have " +
				"changed; its prompts from there on are NOT copied until Livy reads it")
		}
		for _, pr := range read.Found {
			t, ok := when(pr.TS)
			switch {
			case seen[pr.ID]: // a resumed session's history copied into a second transcript
			case !ok:
				noTime++
			case t.Before(since):
				outside++
			default:
				seen[pr.ID] = true
				pr.AgentBefore, pr.Exchanges = "", nil // only the prompt's own text is kept
				found = append(found, pr)
			}
		}
		if scope.TimeoutReads != before {
			continue // a prompt git could not scope in time reads as outside: never kept under another repo
		}
		for _, pr := range read.Outside {
			if t, ok := when(pr.TS); ok && !t.Before(since) && !seen[pr.ID] {
				seen[pr.ID] = true
				outsideRepos = append(outsideRepos, pr)
			}
		}
	}
	found = append(found, committedOnly(o, w, outsideRepos)...)
	if unreadable > 0 {
		w.Error(fmt.Sprintf("prompt_vault: backfill: %d unreadable transcript(s) or line(s), not copied", unreadable))
	}
	if noSession > 0 || noTime > 0 {
		w.Error(fmt.Sprintf("prompt_vault: backfill: %d prompt(s) with no session and %d with no readable time, not copied", noSession, noTime))
	}
	if n := len(scope.TimedOutDirs); n > 0 {
		w.Error(fmt.Sprintf("prompt_vault: backfill: git timed out scoping %d working folder(s); their prompts are not copied "+
			"(a rerun retries them)", n))
		sayTimedOut(o.Stderr, scope.TimedOutDirs, scope.Git.Timeout())
	}
	k := keepText(o, w, found)
	counts["prompts"], counts["outside_days"] = len(found)+outside, outside
	counts["kept"], counts["already"], counts["empty"], counts["too_old"] = k.Kept, k.Already, k.Empty, k.TooOld
	counts["held_files"], counts["unreadable"], counts["scope_timeouts"] = held, unreadable, scope.TimeoutReads
	counts["no_session"], counts["no_time"] = noSession, noTime
	counts["not_kept"] = len(found) - k.Kept - k.Already - k.Empty - k.TooOld
	return counts
}

// backfillGitTimeout is backfill's per-call git timeout in seconds when LIVY_GIT_TIMEOUT is unset.
const backfillGitTimeout = 3.0

// sayTimedOut names the folders git could not scope in time, on local stderr only: a folder
// path never goes in the ledger.
func sayTimedOut(stderr io.Writer, dirs map[string]bool, timeout float64) {
	longer := ""
	if timeout < gitinfo.TimeoutMax {
		longer = fmt.Sprintf(", and %s=%s gives git %s s per call instead of %s s", gitinfo.TimeoutEnv,
			gitinfo.FormatSeconds(gitinfo.TimeoutMax), gitinfo.FormatSeconds(gitinfo.TimeoutMax), gitinfo.FormatSeconds(timeout))
	}
	fmt.Fprintf(stderr, "livy vault backfill: git timed out scoping these folders, so their prompts were not copied; "+
		"a rerun retries them%s:\n", longer)
	for _, d := range slices.Sorted(maps.Keys(dirs)) {
		fmt.Fprintf(stderr, "  %s\n", d)
	}
}

// VaultMain is `livy vault backfill [--days N]`; it returns the exit code: 0 when it copied
// everything it read, 1 when some prompts were not copied or another extraction holds the lock,
// 2 on bad arguments.
func VaultMain(args []string, roots []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "backfill" {
		fmt.Fprintln(stderr, "usage: livy vault backfill [--days N]")
		return 2
	}
	fl := flag.NewFlagSet("livy vault backfill", flag.ContinueOnError)
	fl.SetOutput(stderr)
	days := fl.Int("days", defaultBackfillDays, fmt.Sprintf("copy prompts sent in the last N days (1 to %d)", vault.KeepDays))
	if err := fl.Parse(args[1:]); err != nil {
		return 2
	}
	if fl.NArg() != 0 || *days < 1 || *days > vault.KeepDays {
		fmt.Fprintf(stderr, "livy vault backfill: --days takes 1 to %d (the vault keeps %d days)\n", vault.KeepDays, vault.KeepDays)
		return 2
	}
	dir := hook.DataDir()
	release, err := ledger.Lock(filepath.Join(dir, extractLock), 60*time.Second)
	if err != nil {
		fmt.Fprintln(stderr, "livy vault backfill: an extraction is running; try again when it ends")
		return 1
	}
	defer release()
	counts := VaultBackfill(Options{Roots: roots, CodexRoot: codex.Root(), Now: clock.Now(), Dir: dir, Stderr: stderr}, *days)
	fmt.Fprintln(stdout, dumps(counts, true))
	for _, k := range []string{"not_kept", "held_files", "scope_timeouts", "no_session", "no_time", "unreadable", "empty"} {
		if n, _ := counts[k].(int); n > 0 {
			return 1 // partial: the counts and the ledger.errors say what was not copied
		}
	}
	return 0
}
