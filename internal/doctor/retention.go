package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/0xbew/livy/internal/install"
)

// Claude Code deletes a transcript once it is older than cleanupPeriodDays, 30 days when unset.
// Livy reads skill listings and prompts from transcripts, so a short window loses what a machine
// has not extracted yet (a Livy installed late, an upgrade that reads something new from them).
const (
	claudeCleanupDefault = 30
	cleanupWarnBelow     = 60
	cleanupAdvice        = 90
)

// checkTranscriptRetention warns when Claude Code keeps transcripts fewer than cleanupWarnBelow
// days: unset (Claude Code's default), or set lower, in the settings file Livy's hooks are in.
func checkTranscriptRetention(r *Report) {
	_, settingsPath := install.Recorded()
	days, set, err := cleanupPeriodDays(settingsPath)
	if err != nil {
		r.add(Warn, "transcript retention", "%s could not be read for cleanupPeriodDays (%v): how long Claude Code keeps transcripts is unknown", settingsPath, err)
		return
	}
	if set && days >= cleanupWarnBelow {
		r.add(OK, "transcript retention", "Claude Code keeps transcripts %d days (cleanupPeriodDays)", days)
		return
	}
	why := fmt.Sprintf("cleanupPeriodDays is %d", days)
	if !set {
		why, days = "cleanupPeriodDays is unset", claudeCleanupDefault
	}
	r.add(Warn, "transcript retention", "%s: Claude Code deletes transcripts older than %d days, losing skill and prompt history "+
		"Livy has not extracted yet; set \"cleanupPeriodDays\": %d in %s", why, days, cleanupAdvice, settingsPath)
}

// cleanupPeriodDays is the settings file's cleanupPeriodDays, and whether it is set; a missing
// file sets nothing.
func cleanupPeriodDays(path string) (days int, set bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read: %w", err)
	}
	var doc struct {
		Days *float64 `json:"cleanupPeriodDays"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, false, fmt.Errorf("parse: %w", err)
	}
	if doc.Days == nil {
		return 0, false, nil
	}
	return int(*doc.Days), true, nil
}
