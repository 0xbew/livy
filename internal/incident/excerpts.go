package incident

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/show"
	"github.com/0xbew/livy/internal/why"
)

// excerpts is the pack's excerpts section: each commit's kept prompt text, scrubbed and audited
// through `livy show`'s path, each commit's reveal logged before its text is written. An error
// means no excerpt may be written (no denylist, or a reveal that could not be logged).
func excerpts(commits []why.Explained, stderr io.Writer) (string, error) {
	var b strings.Builder
	b.WriteString("## Excerpts (scrubbed)\n\nAsked for with --excerpts: each prompt's kept text as `livy show` would " +
		"share it, scrubbed with this machine's denylist and audited. Each reveal is in the ledger as prompt.revealed " +
		"(prompt ids and the commit, never the text).\n\n")
	unreadSaid := false // the vault is the same for every commit: say it could not be read once
	for _, c := range commits {
		ids := promptIDs(c.Sessions())
		if len(ids) == 0 {
			continue
		}
		r, err := show.Reveal(ids, c.Commit.SHA, stderr)
		if err != nil {
			return "", fmt.Errorf("excerpts for %s: %w", why.Short(c.Commit.SHA), err)
		}
		fmt.Fprintf(&b, "### `%s`\n\n", why.Short(c.Commit.SHA))
		for _, e := range r.Shown {
			fmt.Fprintf(&b, "- `%s` · %s · branch %s\n\n%s\n\n", name(e.PromptID), e.TS.UTC().Format(time.RFC3339),
				nameOrNone(e.Branch), fenced(e.Text))
		}
		for _, id := range slices.Sorted(maps.Keys(r.Refused)) {
			fmt.Fprintf(&b, "- `%s`: not shown, the audit still finds %s after scrubbing\n", name(id), strings.Join(r.Refused[id], ", "))
		}
		switch {
		case len(r.Missing) > 0 && r.Unread != nil:
			fmt.Fprintf(&b, "- not in the part of this machine's vault that could be read: %s\n", ticks(r.Missing))
		case len(r.Missing) > 0:
			fmt.Fprintf(&b, "- not in this machine's vault: %s\n", ticks(r.Missing))
		}
		if r.Unread != nil && !unreadSaid {
			unreadSaid = true
			fmt.Fprintf(stderr, "livy incident: part of the prompt vault could not be read (%v)\n", r.Unread)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// promptIDs is each prompt id the sessions name, agent-written briefs included, once.
func promptIDs(sessions []why.Session) []string {
	var ids []string
	for _, s := range sessions {
		for _, id := range slices.Concat(s.Prompts, s.AgentBriefs) {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// fenced is text in a Markdown code fence longer than any run of backticks inside it, so the
// text cannot close the fence and become Markdown.
func fenced(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
			continue
		}
		run = 0
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "text\n" + strings.TrimRight(text, "\n") + "\n" + fence
}
