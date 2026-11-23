// Package incident is `livy incident <id> --range A..B`: a Markdown evidence pack for an
// incident review, built from `livy why` over every commit in a deploy range. Text-free: ids,
// names and counts only.
package incident

import (
	"slices"

	"github.com/0xbew/livy/internal/why"
)

// Totals is the range at a glance.
type Totals struct {
	Commits      int
	AgentCommits int      // commits with at least one agent session
	Sessions     []string // each session once, in order of first commit
	Prompts      int      // distinct prompt ids across the range
	Tickets      []string // each ticket key once, no-ticket included, sorted
	NoAgent      []string // short SHAs of commits with no agent session found
}

// total adds up the reports of a range, one per commit in range order.
func total(reports []why.Report) Totals {
	t := Totals{Commits: len(reports)}
	prompts := map[string]bool{}
	for _, r := range reports {
		if len(r.Sessions) == 0 {
			t.NoAgent = append(t.NoAgent, why.Short(r.SHA))
			continue
		}
		t.AgentCommits++
		for _, s := range r.Sessions {
			if !slices.Contains(t.Sessions, s.ID) {
				t.Sessions = append(t.Sessions, s.ID)
			}
			if s.Ticket != "" && !slices.Contains(t.Tickets, s.Ticket) {
				t.Tickets = append(t.Tickets, s.Ticket)
			}
			for _, p := range s.Prompts {
				prompts[p] = true
			}
		}
	}
	t.Prompts = len(prompts)
	slices.Sort(t.Tickets)
	return t
}
