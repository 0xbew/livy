package profile

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/0xbew/livy/internal/why"
)

// earlierRead is how many of a session's earlier commits in this clone are read for files
// changed again.
const earlierRead = 50

// fixupPrefixes start the subject of a commit made to be folded into another by an autosquash.
var fixupPrefixes = []string{"fixup! ", "squash! ", "amend! "}

// rework is what Livy can see of a commit being redone: it is a fix-up, it was rewritten
// (amend, rebase), and it changes files its session's earlier commits changed.
type rework struct {
	fixup    bool
	rewrites map[string]int // rewrite kind -> how many times
	// again is how many of the commit's files earlier commits of its sessions changed, and
	// earlier how many such commits there were; problem says why they could not be read.
	again, earlier int
	problem        string
}

func (rw rework) String() string {
	var parts []string
	if rw.fixup {
		parts = append(parts, "a fix-up commit")
	}
	if len(rw.rewrites) > 0 {
		var kinds []string
		for _, k := range slices.Sorted(maps.Keys(rw.rewrites)) {
			kinds = append(kinds, fmt.Sprintf("%s %d", k, rw.rewrites[k]))
		}
		parts = append(parts, "rewritten ("+strings.Join(kinds, ", ")+")")
	}
	switch {
	case rw.problem != "":
		parts = append(parts, "files changed again not read ("+rw.problem+")")
	case rw.again > 0:
		parts = append(parts, fmt.Sprintf("changes %d files again (earlier commits of its session that changed them: %d)", rw.again, rw.earlier))
	}
	if len(parts) == 0 {
		return "none seen"
	}
	return strings.Join(parts, "; ")
}

// reworkOf reads c's rework signals from git and the ledger events.
func reworkOf(c why.Commit, sessions []why.Session, events []why.Event, cf *commitFiles) rework {
	rw := rework{rewrites: map[string]int{}}
	subject, err := why.Git("log", "-1", "--no-show-signature", "--format=%s", c.SHA, "--")
	if err == nil {
		for _, p := range fixupPrefixes {
			rw.fixup = rw.fixup || strings.HasPrefix(subject, p)
		}
	}
	aliases := why.Aliases(c.SHA, events)
	for _, e := range events {
		if e.Event == "commit.rewritten" && slices.Contains(aliases, e.CommitSHA) && slices.Contains(aliases, e.OldCommitSHA) {
			rw.rewrites[e.RewriteKind]++
		}
	}
	earlier := earlierCommits(c, sessions, aliases, events)
	if len(earlier) == 0 {
		return rw
	}
	if err := cf.know(append([]string{c.SHA}, earlier...)); err != nil {
		rw.problem = "git could not list the commits' files"
		return rw
	}
	mine, ok := cf.files(c.SHA)
	if !ok {
		rw.problem = "git could not list this commit's files"
		return rw
	}
	rw.again, rw.earlier = filesAgain(mine, earlier, cf)
	return rw
}

// filesAgain is how many of mine the first earlierRead of earlier (those this clone has)
// changed too, and how many of those commits changed any.
func filesAgain(mine map[string]bool, earlier []string, cf *commitFiles) (again, commits int) {
	seen := map[string]bool{}
	read := 0
	for _, sha := range earlier {
		if read == earlierRead {
			break
		}
		files, ok := cf.files(sha)
		if !ok {
			continue // a commit this clone lacks: another repo's, or a rebased-away pick
		}
		read++
		touched := false
		for f := range files {
			if mine[f] {
				seen[f], touched = true, true
			}
		}
		commits += b2i(touched)
	}
	return len(seen), commits
}

// earlierCommits are the commits made before c by c's sessions: on its first-parent line by
// trailer, and by the ledger's commit.recorded lines; not c itself, not a commit that was
// rewritten into another.
func earlierCommits(c why.Commit, sessions []why.Session, aliases []string, events []why.Event) []string {
	skip := map[string]bool{}
	for _, a := range aliases {
		skip[a] = true
	}
	// A commit later amended or rebased is counted as the commit it became.
	for _, e := range events {
		if e.Event == "commit.rewritten" {
			skip[e.OldCommitSHA] = true
		}
	}
	var out []string
	for _, s := range sessions {
		for _, sha := range sessionCommits(c, s, events) {
			if !skip[sha] && !slices.Contains(out, sha) {
				out = append(out, sha)
			}
		}
	}
	return out
}

// sessionCommits are the commits session s made before its window ends: those on c's
// first-parent line its trailer names, then those its commit.recorded lines name.
func sessionCommits(c why.Commit, s why.Session, events []why.Event) []string {
	var out []string
	for _, a := range c.Ancestors {
		if slices.Contains(a.Sessions, s.ID) {
			out = append(out, a.SHA)
		}
	}
	for _, e := range events {
		if e.Event != "commit.recorded" || e.SessionID != s.ID {
			continue
		}
		if t, ok := why.ParseTS(e.TS); ok && t.Before(s.To) {
			out = append(out, e.CommitSHA)
		}
	}
	return out
}
