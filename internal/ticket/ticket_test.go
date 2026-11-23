package ticket

import "testing"

// Guards: agent work that cannot be grouped by ticket. The key must come from the branch
// before the commit message, be one spelling whatever the case it was written in, take a
// pattern's capture group when it has one, and be the explicit None, never "", when nothing
// matches. Branch names are shapes seen in Jira-linked repos.
func TestKey(t *testing.T) {
	for _, c := range []struct {
		name, pattern string
		sources       []string
		want          string
	}{
		{"jira branch", DefaultRegex, []string{"feature/PROJ-123-add-login"}, "PROJ-123"},
		{"underscore after the key", DefaultRegex, []string{"feature/PROJ-123_add_login"}, "PROJ-123"},
		{"branch before message", DefaultRegex, []string{"bugfix/OPS-7", "PROJ-9: fix it"}, "OPS-7"},
		{"message when the branch has none", DefaultRegex, []string{"main", "PROJ-9: fix it"}, "PROJ-9"},
		{"nothing matches", DefaultRegex, []string{"telemetry/followup", "fix: a thing"}, None},
		{"no sources", DefaultRegex, nil, None},
		{"lower-case key is one spelling", `(?i)[a-z][a-z0-9]+-\d+`, []string{"proj-42-thing"}, "PROJ-42"},
		{"capture group", `(?i)gh-(\d+)`, []string{"fix/gh-310"}, "310"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, err := New(c.pattern)
			if err != nil {
				t.Fatalf("New(%q): %v", c.pattern, err)
			}
			if got := m.Key(c.sources...); got != c.want {
				t.Errorf("Key(%q) with %q = %q, want %q", c.sources, c.pattern, got, c.want)
			}
		})
	}
}
