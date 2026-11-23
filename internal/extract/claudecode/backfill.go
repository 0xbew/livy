package claudecode

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/0xbew/livy/internal/extract"
)

// BackfillSource marks a usage line this pass wrote again, so derive can tell it from the
// extractor's own copy of the call.
const BackfillSource = "transcript-backfill"

// backfillVias are the target_via values, each with a count in the run's stats.
var backfillVias = []string{"cd", "git_c", "file_path", extract.ViaSession}

// Backfill reads every transcript from its first byte and writes each in-scope call's
// usage.api_call line again, with target_via, target_repo and target_branch and source
// `transcript-backfill`. It writes no other event, and it never reads, changes or consults
// the extractor's state or its log of lines written: a call it writes again must not make the
// extractor skip that call's live line, and a second run appends everything again (derive
// keeps one copy per message_id).
func Backfill(o Options) Stats {
	scope := extract.NewScope(o.Stderr)
	b := &usageBackfill{
		o:     o,
		stats: newBackfillStats(),
		w:     &extract.Writer{Dir: o.Dir, Stderr: o.Stderr, DryRun: o.DryRun},
		scope: scope,
		rd:    &reader{scope: scope},
		seen:  map[string]bool{},
	}
	done := map[string]bool{}
	for _, root := range o.Roots {
		if root == "" {
			continue
		}
		real := resolvedRoot(root)
		if done[real] { // a root named twice is read once
			continue
		}
		done[real] = true
		for _, tf := range transcriptFiles(real, &b.unreadable) {
			if !b.readFile(tf.path) {
				return b.finish()
			}
		}
	}
	return b.finish()
}

// usageBackfill is one Backfill run's state, shared by every transcript it reads.
type usageBackfill struct {
	o          Options
	stats      Stats
	w          *extract.Writer
	scope      *extract.Scope
	rd         *reader
	seen       map[string]bool // message ids already re-stated, across every root
	unreadable int             // transcript directories that could not be listed
	stopped    string          // why the run stopped early, or ""
}

// newBackfillStats starts every reported count at zero, so the report names each one.
func newBackfillStats() Stats {
	stats := Stats{}
	for _, k := range []string{"files", "files_read", "calls", "out_of_scope", "synthetic", "written",
		"target_unresolved", "target_branch_unknown", "target_differs", "unusable_records", "bad_lines",
		"unparseable_ts", "unreadable_files", "unreadable_dirs", "scope_held_files", "invalid",
		"failed_writes", "unsettled_files"} {
		stats[k] = 0
	}
	for _, via := range backfillVias {
		stats["via_"+via] = 0
	}
	return stats
}

// resolvedRoot is a root's absolute path with its symlinks followed, as far as they resolve.
func resolvedRoot(root string) string {
	real, err := filepath.Abs(root)
	if err != nil {
		real = filepath.Clean(root)
	}
	if r, err := filepath.EvalSymlinks(real); err == nil {
		real = r
	}
	return real
}

// readFile re-states one transcript's calls. It returns false when a write failed and the run
// must stop.
func (b *usageBackfill) readFile(path string) bool {
	b.stats["files"]++
	p, ok := b.read(path)
	if !ok {
		return true
	}
	// Counted after the read: only a timeout while placing this file's calls holds it.
	timeoutsBefore := b.scope.TimeoutReads
	b.stats["files_read"]++
	before := Stats{}
	maps.Copy(before, b.stats)
	b.stats["synthetic"] += p.synthetic
	b.stats["bad_lines"] += p.badLines
	b.stats["unusable_records"] += p.assistant - p.usable
	lines, fileSeen := b.targeted(p)
	if b.scope.TimeoutReads != timeoutsBefore {
		b.hold(before, fileSeen)
		return true
	}
	b.stats["calls"] += len(lines)
	return b.write(lines)
}

// read is a whole transcript's usage, read from byte 0; false when it is gone or unreadable.
func (b *usageBackfill) read(path string) (*parsed, bool) {
	st, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			b.stats["unreadable_files"]++
		}
		return nil, false
	}
	settled := b.o.Now.Sub(st.ModTime()) >= settleSeconds*time.Second
	if !settled {
		b.stats["unsettled_files"]++
	}
	// Usage from byte 0; every other family starts at the end, so none of them is collected.
	size := st.Size()
	p := b.rd.readTranscript(path, 0, settled, size, nil, size, nil, size, nil)
	if p.readError {
		b.stats["unreadable_files"]++
		return nil, false
	}
	return p, true
}

// write appends lines in batches; false when one failed, with why the run stopped.
func (b *usageBackfill) write(lines []map[string]any) bool {
	for i := 0; i < len(lines); i += batchLines {
		if !b.w.Write(lines[i:min(i+batchLines, len(lines))]) {
			b.stopped = fmt.Sprintf("%d ledger line(s) could not be written", b.w.Failed)
			return false
		}
	}
	return true
}

// targeted builds the backfill line for each call in p not already seen, and returns the
// message ids this file marked seen.
func (b *usageBackfill) targeted(p *parsed) ([]map[string]any, []string) {
	var fileSeen []string
	var lines []map[string]any
	// The first copy of a call wins, live root first: the copy the extractor itself wrote.
	for _, k := range p.messages {
		id := k.rec["message"].(map[string]any)["id"].(string)
		if b.seen[id] {
			continue
		}
		b.seen[id] = true
		fileSeen = append(fileSeen, id)
		cwd, haveCwd := cwdOf(k.rec)
		placed, in := b.scope.Place(cwd, haveCwd, k.named)
		if !in {
			b.stats["out_of_scope"]++
			continue
		}
		ts := extract.UTCSeconds(k.rec["timestamp"])
		if ts == "" {
			b.stats["unparseable_ts"]++
			continue
		}
		line := targetedUsage(b.scope, k, cwd, placed, ts)
		line["source"] = BackfillSource
		tally(b.stats, line)
		lines = append(lines, line)
	}
	return lines, fileSeen
}

// hold puts the counts back as they were before a file whose calls' scope is unknown, so none
// of it is written, as if it had not been read, and counts the file as held.
func (b *usageBackfill) hold(before Stats, fileSeen []string) {
	held := before["scope_held_files"] + 1
	for k := range b.stats {
		delete(b.stats, k)
	}
	maps.Copy(b.stats, before)
	b.stats["scope_held_files"] = held
	for _, id := range fileSeen {
		delete(b.seen, id)
	}
}

// finish fills in the writer's counts, reports every problem as one ledger.error each and
// returns the run's stats.
func (b *usageBackfill) finish() Stats {
	stats := b.stats
	stats["unreadable_dirs"] = b.unreadable
	stats["written"], stats["invalid"], stats["failed_writes"] = b.w.Written, b.w.Invalid, b.w.Failed
	for _, r := range b.reasons() {
		b.w.Error("usage-backfill: " + r)
	}
	return stats
}

// reasons are the run's problems, worded for its ledger.error lines, in a fixed order.
func (b *usageBackfill) reasons() []string {
	stats := b.stats
	var reasons []string
	say := func(key, format string) {
		if stats[key] > 0 {
			reasons = append(reasons, fmt.Sprintf(format, stats[key]))
		}
	}
	say("unusable_records", "%d assistant record(s) carried no usable id or token counts; the transcript format may have changed, and those calls are NOT re-stated")
	say("bad_lines", "%d transcript line(s) were not JSON objects; any call on them is NOT re-stated")
	say("unparseable_ts", "%d call(s) had an unparseable timestamp and were NOT re-stated")
	say("unreadable_files", "%d transcript file(s) could not be read; their calls were NOT re-stated")
	say("unreadable_dirs", "%d transcript director(y/ies) could not be listed")
	say("scope_held_files", "%d transcript file(s) were skipped: git timed out reading a working directory's origin, so their calls' scope is unknown; run again to re-state them")
	say("invalid", "%d line(s) broke the ledger line rules and were not written")
	say("failed_writes", "%d ledger line(s) failed to write")
	say("unsettled_files", "%d transcript file(s) were written to in the last 10 minutes: a call still streaming there is left to the extractor, which records its target itself")
	var problems []string
	for r := range b.scope.Problems {
		problems = append(problems, r)
	}
	sort.Strings(problems)
	for _, r := range problems {
		reasons = append(reasons, fmt.Sprintf("%d call(s): %s", b.scope.Problems[r], r))
	}
	if b.stopped != "" {
		reasons = append(reasons, "stopped: "+b.stopped+"; run again to re-state the rest")
	}
	return reasons
}

// tally counts where the calls worked, for the report. It never holds a path.
func tally(stats Stats, line map[string]any) {
	via, _ := line["target_via"].(string)
	stats["via_"+via]++
	repo, hasRepo := line["target_repo"].(string)
	switch {
	case !hasRepo:
		stats["target_unresolved"]++
	case line["target_branch"] == extract.Unknown:
		stats["target_branch_unknown"]++
	case repo != line["repo"] || line["target_branch"] != line["branch"]:
		stats["target_differs"]++
	}
}
