package doctor

import (
	"strings"
	"testing"
	"time"
)

// Guards: a Codex process started before its export moved to the relay goes on exporting
// with what it read at start, which the collector refuses, with nothing on the laptop to
// say so. Rows are real `ps -A -o pid=,etime=,comm=` shapes.
func TestStaleCodex(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	since := now.Add(-2 * time.Hour)
	ps := strings.Join([]string{
		"  101 3-04:05:06 /Applications/Codex.app/Contents/Resources/codex", // days old
		"  102    02:30:00 codex", // started 2.5 h ago
		"  103       59:00 codex", // started after the move
		"  104    05:00:00 /Applications/Codex.app/Contents/Resources/codex-code-mode-host",
		"  105    05:00:00 /usr/bin/zsh",
	}, "\n")
	if got := strings.Join(StaleCodex(ps, now, since), ","); got != "101,102" {
		t.Errorf("stale Codex pids %q, want 101,102", got)
	}
}
