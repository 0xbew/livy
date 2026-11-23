package denylist

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/0xbew/scrub"

	"github.com/0xbew/livy/internal/config"
)

// Doctor's statuses (the same words as internal/doctor's).
const (
	StatusOK   = "ok"
	StatusWarn = "warn"
	StatusFail = "fail"
)

// StaleAfter: the daily flush confirms the copy against a remote source; unconfirmed this
// long, its refresh has stopped.
const StaleAfter = 3 * 24 * time.Hour

// Check is doctor's `denylist` row: the copy read the way every sender reads it, and the
// last refresh. No copy is a warning (names are not scrubbed); a copy that cannot be used is
// a failure (spot-checks then send nothing). A failed or refused refresh is said beside a
// copy that still works: the daily job's output goes nowhere, so this row is where it shows.
// A copy kept by hand (no source) or copied from a local file is never stale.
func Check(res config.Resolved, dataDir string, now time.Time) (status, detail string) {
	path := Path(res, dataDir)
	shown := home(path)
	src, srcErr := FromSettings(res)
	last, _ := ReadRecord(dataDir)
	refresh := refreshProblem(srcErr, last)
	d, err := scrub.LoadDenylist(path)
	switch {
	case errors.Is(err, scrub.ErrDenylistMissing):
		return StatusWarn, fmt.Sprintf("none at %s, so names are not scrubbed; %s", shown, whyMissing(refresh, src, last))
	case err != nil: // its message never quotes an entry
		return StatusFail, fmt.Sprintf("%s is unusable (%v): spot-checks send nothing until it is replaced; "+
			"run `livy install --refresh-denylist`", shown, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return StatusFail, fmt.Sprintf("%s cannot be read (%s)", shown, pathless(err))
	}
	var problems []string
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		problems = append(problems, fmt.Sprintf("mode %04o: chmod 600 it, it names people", perm))
	}
	var confirmedAgo, confirmedWhy = time.Duration(0), ""
	if src.remote() {
		confirmedAgo, confirmedWhy = confirmation(last, src, path, now)
		if confirmedWhy != "" {
			problems = append(problems, confirmedWhy)
		} else if confirmedAgo > StaleAfter {
			problems = append(problems, fmt.Sprintf("not confirmed for %.0fd: the daily flush has stopped refreshing it", confirmedAgo.Hours()/24))
		}
	}
	if refresh != "" {
		problems = append(problems, refresh)
	}
	switch {
	case src == nil:
		detail = fmt.Sprintf("v%d, kept by hand (no denylist.source)", d.Version)
	case src.remote():
		detail = fmt.Sprintf("v%d, confirmed against the %s source %.0fh ago", d.Version, src.Type, confirmedAgo.Hours())
		if confirmedWhy != "" {
			detail = fmt.Sprintf("v%d, NOT confirmed against the %s source", d.Version, src.Type)
		}
	default:
		detail = fmt.Sprintf("v%d, copied from a local file", d.Version)
	}
	if len(problems) > 0 {
		return StatusWarn, detail + "; " + strings.Join(problems, "; ")
	}
	return StatusOK, detail
}

// refreshProblem is what is wrong with the source or the last refresh, or "".
func refreshProblem(srcErr error, last Record) string {
	switch {
	case srcErr != nil:
		return "the source cannot be read: " + srcErr.Error()
	case last.Outcome == Failed || last.Outcome == Refused:
		verb := "FAILED"
		if last.Outcome == Refused {
			verb = "REFUSED a version"
		}
		return fmt.Sprintf("the last refresh %s at %s: %s", verb, last.CheckedAt, deref(last.Reason))
	}
	return ""
}

// whyMissing is why there is no local copy: the refresh problem, else no source, else an
// empty secret, else no refresh yet.
func whyMissing(refresh string, src *Source, last Record) string {
	switch {
	case refresh != "":
		return refresh
	case src == nil:
		return "no denylist.source is configured"
	case last.Outcome == NoVersion:
		return "the last refresh found " + deref(last.Reason)
	}
	return "no refresh has run: run `livy install --refresh-denylist`"
}

// confirmation is how long ago the copy on disk was last confirmed against src, from the
// refresh record alone. why is set when it was not: no successful refresh recorded, a
// different source since, or a copy that is not the one that refresh left.
func confirmation(last Record, src *Source, path string, now time.Time) (age time.Duration, why string) {
	at, err := time.Parse(time.RFC3339, last.LastOKAt)
	switch {
	case last.LastOKAt == "" || err != nil:
		return 0, "no successful refresh is recorded: run `livy install --refresh-denylist`"
	case last.LastOKSource != sourceID(src):
		return 0, "last confirmed against another source: run `livy install --refresh-denylist`"
	case last.LastOKSHA256 != copyHash(path):
		return 0, "the copy differs from the copy last confirmed: run `livy install --refresh-denylist`"
	}
	return max(0, now.Sub(at)), ""
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
