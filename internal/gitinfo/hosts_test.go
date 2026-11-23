package gitinfo

import (
	"io"
	"testing"
)

// Guards: a GitHub Enterprise or GitLab team that configures its host and still records
// nothing (rule 17: in scope = a configured org on a configured host), and a configured
// host list that leaves github.com in scope anyway. https remotes only: no ssh lookup.
func TestScopeIsAConfiguredOrgOnAConfiguredHost(t *testing.T) {
	orgs := func() []string { return []string{"acme"} }
	for _, c := range []struct {
		hosts []string
		url   string
		want  bool
	}{
		{nil, "https://github.com/acme/w.git", true},
		{nil, "https://github.example.com/acme/w.git", false},
		{[]string{"github.example.com"}, "https://github.example.com/acme/w.git", true},
		{[]string{"GitHub.Example.com"}, "https://github.example.com/Acme/w", true},
		{[]string{"github.example.com"}, "https://github.com/acme/w.git", false},
		{[]string{"github.example.com"}, "https://github.example.com/other/w.git", false},
	} {
		g := New(io.Discard)
		g.Hosts = c.hosts
		if got := g.InScope(c.url, orgs); got != c.want {
			t.Errorf("hosts %v, %s: InScope = %v, want %v", c.hosts, c.url, got, c.want)
		}
	}
}
