// Package cli is `livy extract`: one pass of every transcript extractor (Claude Code, then
// Codex), under one lock.
//
//	livy extract                 incremental pass over the live transcripts
//	livy extract --root DIR      also read DIR (an archive of Claude Code projects)
//	livy extract --dry-run       count what would be written; write nothing
//	livy extract --throttle S    do nothing if a pass ran less than S seconds ago
//	livy extract --usage-backfill  write every Claude Code usage line again, with where each call
//	                             worked; touches no extractor state (--root and --dry-run apply)
//	livy extract --tool-calls-backfill
//	                             one tool.call line per tool call in the transcripts, under its
//	                             own lock and state; nothing else runs (--root and --dry-run apply)
//
// It never blocks a session: every path exits 0, and a problem becomes a ledger.error.
// Counts go to stdout as one JSON object.
package cli

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/extract"
	"github.com/0xbew/livy/internal/extract/claudecode"
	"github.com/0xbew/livy/internal/extract/codex"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/oldnames"
	"github.com/0xbew/livy/internal/prompts"
)

const (
	lockName = "extract.lock"
	// Its own lock, so a backfill runs beside the live extractor.
	backfillLockName = "usage-backfill.lock"
	lastRunName      = "extract-last-run"
	// A run that left work behind retries sooner than the throttle, but not on every prompt.
	retryAfter = 120 * time.Second
)

// rootsEnv names extra transcript roots: the Python tool's variable, kept so a migrated
// machine's roots still apply; "" in a build with none.
var rootsEnv = oldnames.RecorderEnv("TRANSCRIPT_ROOTS")

// DataNames are the files and folders this package keeps in the data folder, by exact name.
// `livy uninstall --purge` deletes these and nothing else.
func DataNames() []string { return []string{lockName, backfillLockName, lastRunName} }

type args struct {
	roots     []string
	dryRun    bool
	backfill  bool // --usage-backfill
	toolCalls bool // --tool-calls-backfill
	throttle  *float64
	// noPromptPass skips prompt capture: emit runs extraction inside its lock, and a relay proof
	// reads two weeks of every project's transcripts.
	noPromptPass bool
}

func parse(argv []string) (args, string) {
	var a args
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--dry-run":
			a.dryRun = true
		case "--usage-backfill":
			a.backfill = true
		case "--tool-calls-backfill":
			a.toolCalls = true
		case "--no-prompt-pass":
			a.noPromptPass = true
		case "--root", "--throttle":
			if !hasValue {
				if i+1 >= len(argv) {
					return a, name + " needs a value"
				}
				i++
				value = argv[i]
			}
			if name == "--root" {
				a.roots = append(a.roots, value)
				continue
			}
			f, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				return a, "--throttle needs a number of seconds"
			}
			a.throttle = &f
		default:
			return a, "unknown argument " + strconv.Quote(arg)
		}
	}
	return a, conflict(a)
}

// conflict is why flags that each parsed cannot run together, or "".
func conflict(a args) string {
	switch {
	case a.backfill && a.toolCalls:
		return "--usage-backfill and --tool-calls-backfill are separate runs"
	case a.backfill && a.throttle != nil:
		return "--throttle does not apply to --usage-backfill"
	case a.toolCalls && a.throttle != nil:
		return "--throttle does not apply to --tool-calls-backfill"
	}
	return ""
}

// Main runs one extract pass and returns the exit code, which is always 0.
func Main(argv []string, stdout, stderr io.Writer) (code int) {
	dir := hook.DataDir()
	w := &extract.Writer{Dir: dir, Stderr: stderr}
	backfilling, dryRun := "", false
	defer func() {
		if p := recover(); p != nil {
			code = 0
			defer func() { _ = recover() }()
			what := "extract"
			if backfilling != "" {
				what = backfilling
				w.DryRun = dryRun // a dry run writes nothing, not even this
			}
			w.Error(fmt.Sprintf("%s: unhandled panic (%T)", what, p)) // the value may carry a path
		}
	}()
	a, bad := parse(argv)
	if bad != "" {
		w.Error("extract: bad arguments: " + bad)
		return 0
	}
	if a.backfill {
		backfilling, dryRun = "usage-backfill", a.dryRun
		return backfill(a, dir, stdout, stderr)
	}
	if a.toolCalls {
		backfilling, dryRun = "tool-backfill", a.dryRun
		return toolCallsBackfill(a, dir, stdout, stderr)
	}
	stamp := filepath.Join(dir, lastRunName)
	if a.throttle != nil {
		if st, err := os.Stat(stamp); err == nil && clock.Now().Sub(st.ModTime()).Seconds() < *a.throttle {
			return 0 // quiet on purpose: this runs inside every mid-session flush
		}
	}
	return incremental(a, dir, stamp, stdout, stderr)
}

// incremental is one live pass, under the extractor's lock: Claude Code, Codex, then prompt
// capture, and the throttle stamp.
func incremental(a args, dir, stamp string, stdout, stderr io.Writer) int {
	unlock, ok := lock(dir, lockName)
	if !ok {
		fmt.Fprintln(stderr, "extract: another extraction is running; leaving it to that one")
		return 0
	}
	defer unlock()
	stats := claudecode.Run(claudecode.Options{Roots: roots(a), DryRun: a.dryRun, Dir: dir, Stderr: stderr, Now: clock.Now()})
	cw := &extract.Writer{Dir: dir, Stderr: stderr, DryRun: a.dryRun}
	for k, v := range codex.Run(codex.Root(), dir, cw, stderr) {
		stats["codex_"+k] = v
	}
	if a.throttle == nil && !a.noPromptPass {
		prompts.Run(prompts.Options{Roots: roots(a), CodexRoot: codex.Root(), DryRun: a.dryRun, Now: clock.Now(),
			Dir: dir, Stderr: stderr})
	}
	stats["written"] += cw.Written
	stats["invalid"] += cw.Invalid
	stats["failed_writes"] += cw.Failed
	if !a.dryRun {
		if err := extract.Touch(stamp, stampAge(a, stats)); err != nil {
			fmt.Fprintf(stderr, "extract: could not update %s (%T)\n", lastRunName, err)
		}
	}
	fmt.Fprintln(stdout, claudecode.MarshalStats(stats))
	return 0
}

// PromptPass is one prompt capture pass under the extractor's lock, for emit --extract: the
// daily job and the throttled flush, so prompts are captured on a machine where Claude Code's
// SessionStart never runs (Codex or Amp only). The pass keeps its own hourly gate; a lock
// another extraction holds leaves the pass to that one.
func PromptPass(stderr io.Writer) {
	dir := hook.DataDir()
	unlock, ok := lock(dir, lockName)
	if !ok {
		return // quiet on purpose: the extraction holding the lock runs its own pass
	}
	defer unlock()
	prompts.Run(prompts.Options{Roots: roots(args{}), CodexRoot: codex.Root(), Now: clock.Now(), Dir: dir, Stderr: stderr})
}

// stampAge backdates the throttle stamp after a failed write, so the next pass retries
// sooner than the throttle would allow.
func stampAge(a args, stats claudecode.Stats) time.Duration {
	if stats["failed_writes"] <= 0 && stats["state_write_failures"] <= 0 && stats["codex_state_write_failures"] <= 0 {
		return 0
	}
	t := 0.0
	if a.throttle != nil {
		t = *a.throttle
	}
	return max(0, time.Duration(t*float64(time.Second))-retryAfter)
}

// Roots is the transcript roots a prompt pass reads: the live root, the roots named in the
// environment, then extra.
func Roots(extra []string) []string { return roots(args{roots: extra}) }

// roots is the live root, then the roots named in the environment, then --root.
func roots(a args) []string {
	out := []string{claudecode.LiveRoot()}
	for _, r := range filepath.SplitList(oldnames.Getenv(rootsEnv)) {
		if strings.TrimSpace(r) != "" {
			out = append(out, r)
		}
	}
	return append(out, a.roots...)
}

// backfill is one --usage-backfill pass. It takes its own lock and never the extractor's, and
// it leaves the throttle stamp alone: a backfill must not delay the next live pass.
func backfill(a args, dir string, stdout, stderr io.Writer) int {
	unlock, ok := lock(dir, backfillLockName)
	if !ok {
		fmt.Fprintln(stderr, "usage-backfill: another backfill is running; leaving it to that one")
		return 0
	}
	defer unlock()
	stats := claudecode.Backfill(claudecode.Options{Roots: roots(a), DryRun: a.dryRun, Dir: dir, Stderr: stderr, Now: clock.Now()})
	fmt.Fprintln(stdout, claudecode.MarshalStats(stats))
	return 0
}

// toolCallsBackfill is one tool-call backfill under its own lock, so it never waits on, or
// blocks, the regular extract pass.
func toolCallsBackfill(a args, dir string, stdout, stderr io.Writer) int {
	unlock, ok := lock(dir, claudecode.ToolBackfillLockName)
	if !ok {
		fmt.Fprintln(stderr, "tool-backfill: another backfill is running; leaving it to that one")
		return 0
	}
	defer unlock()
	stats := claudecode.ToolBackfill(claudecode.Options{Roots: roots(a), DryRun: a.dryRun, Dir: dir,
		Stderr: stderr, Now: clock.Now()})
	fmt.Fprintln(stdout, claudecode.MarshalStats(stats))
	return 0
}
