package doctor

import (
	"strings"
	"testing"
)

// Guards: a claude binary whose `--version` output cannot be read counted as meeting the
// minimum, so a machine that may record no model timeline reads as fine.
func TestClaudeVersionRow(t *testing.T) {
	for _, c := range []struct {
		name     string
		bins     map[string]string
		want     string
		wantText string
	}{
		{"current", map[string]string{"/a/claude": "2.1.300 (Claude Code)"}, OK, "all at least 2.1.251"},
		{"old", map[string]string{"/a/claude": "2.1.100 (Claude Code)"}, Warn, "older than 2.1.251"},
		{"unparseable", map[string]string{"/a/claude": "command not found: node"}, Warn, "cannot read"},
		{"one current one unparseable", map[string]string{"/a/claude": "2.1.300 (Claude Code)", "/b/claude": ""}, Warn, "cannot read"},
	} {
		got, detail := claudeVersionRow(c.bins)
		if got != c.want || !strings.Contains(detail, c.wantText) {
			t.Errorf("%s: %s %q, want %s containing %q", c.name, got, detail, c.want, c.wantText)
		}
	}
}
