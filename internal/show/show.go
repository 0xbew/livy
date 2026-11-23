// Package show is `livy show --prompt <id> | --commit <sha>`: the kept text of a prompt, or of
// every prompt behind a commit, scrubbed with the machine's denylist exactly as it would be
// shared. It refuses to run without a denylist, and each reveal writes a prompt.revealed
// ledger line naming the prompt ids, never the text.
package show

import (
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/0xbew/scrub"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/extract"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/prompts"
	"github.com/0xbew/livy/internal/vault"
	"github.com/0xbew/livy/internal/why"
)

const usage = "usage: livy show --prompt <id> | --commit <sha>"

// Main runs `livy show` and returns the exit status: 0 when it ran (whatever it found), 1 when
// it refused, could not log a reveal, or could not read a folder that exists (what it could read
// is still shown), 2 on bad arguments or a commit git does not know.
func Main(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("livy show", flag.ContinueOnError)
	fl.SetOutput(stderr)
	prompt := fl.String("prompt", "", "a prompt id: prompt capture's (prompt:...) or the harness's own")
	commit := fl.String("commit", "", "a commit: every prompt `livy why` finds behind it")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() != 0 || (*prompt == "") == (*commit == "") {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	names, why := prompts.LoadDenylist()
	if names == nil {
		fmt.Fprintf(stderr, "livy show: refused: no usable denylist (%s). Text is shown only after the scrubber "+
			"has your identity list; run `livy install --refresh-denylist` or set denylist.path\n", why)
		return 1
	}
	b, code := idsFor(*prompt, *commit, stdout, stderr)
	if code != 0 {
		return code
	}
	status := 0
	for _, f := range b.Unread {
		fmt.Fprintf(stderr, "livy show: %s could not be read; prompts behind the commit may be missing\n", f)
		status = 1
	}
	if len(b.Prompts) == 0 {
		return status
	}
	r, err := Reveal(b.Prompts, b.SHA, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "livy show: %v; nothing shown\n", err)
		return 1
	}
	write(stdout, r)
	if r.Unread != nil {
		fmt.Fprintf(stderr, "livy show: part of the prompt vault could not be read (%v)\n", r.Unread)
		return 1
	}
	return status
}

// idsFor is the prompt ids to show and the commit they are behind (no SHA for --prompt), or a
// non-zero exit status.
func idsFor(prompt, commit string, stdout, stderr io.Writer) (why.Behind, int) {
	if prompt != "" {
		return why.Behind{Prompts: []string{prompt}}, 0
	}
	b, err := why.CommitPrompts(commit)
	if err != nil {
		fmt.Fprintf(stderr, "livy show: %v\n", err)
		return why.Behind{}, 2
	}
	fmt.Fprintf(stdout, "commit %s: %d prompt(s) behind it, as `livy why` finds them\n", b.SHA, len(b.Prompts))
	return b, 0
}

// shownText is one record with its text, repo and branch as they would be shared.
type shownText struct {
	rec                vault.Record
	text, repo, branch string
}

// judged is what a reveal shows and what the audit refused, with the rules it named.
type judged struct {
	shown   []shownText
	refused map[string][]string
}

// judge scrubs each record's text, repo and branch and audits what is left, as an upload would:
// a record the audit still finds a name or a secret in is not shown.
func judge(recs []vault.Record, names *scrub.Denylist) judged {
	j := judged{refused: map[string][]string{}}
	for _, r := range recs {
		s := shownText{rec: r, text: prompts.Scrubbed(r.Text, names), repo: prompts.Scrubbed(r.Repo, names),
			branch: prompts.Scrubbed(r.Branch, names)}
		fields := map[string]any{"prompt": s.text, "repo": s.repo, "branch": s.branch}
		if hits := scrub.AuditValue(fields, names); len(hits) > 0 {
			j.refused[r.PromptID] = hits
			continue
		}
		j.shown = append(j.shown, s)
	}
	return j
}

// logReveal writes one prompt.revealed line per repo, naming the prompt ids shown and never
// their text; false when any could not be written.
func logReveal(shown []shownText, sha string, names *scrub.Denylist, stderr io.Writer) bool {
	byRepo := map[string][]any{} // []any: the ledger encoder takes JSON-shaped values only
	var repos []string
	for _, s := range shown {
		if _, ok := byRepo[s.rec.Repo]; !ok {
			repos = append(repos, s.rec.Repo)
		}
		byRepo[s.rec.Repo] = append(byRepo[s.rec.Repo], s.rec.PromptID)
	}
	for _, repo := range repos {
		e := map[string]any{"schema_version": int64(extract.SchemaVersion), "ts": clock.Seconds(clock.Now()),
			"event": "prompt.revealed", "repo": repo, "prompt_ids": byRepo[repo],
			"denylist_version": int64(names.Version), "scrubber_version": int64(scrub.Version)}
		if sha != "" {
			e["commit_sha"] = sha
		}
		if !hook.Append(e, stderr) {
			return false
		}
	}
	return true
}

// missing is the ids no kept record answers to.
func missing(ids []string, recs []vault.Record) []string {
	var out []string
	for _, id := range ids {
		if !slices.ContainsFunc(recs, func(r vault.Record) bool { return r.PromptID == id || r.HarnessPromptID == id }) {
			out = append(out, id)
		}
	}
	return out
}

func write(w io.Writer, r Revealed) {
	fmt.Fprintf(w, "What would be shared: scrubbed with denylist v%d and scrubber v%d.\n", r.DenylistVersion, scrub.Version)
	for _, e := range r.Shown {
		fmt.Fprintf(w, "\n--- prompt %s · %s · %s · branch %s · session %s\n%s\n", e.Label(),
			e.TS.UTC().Format(time.RFC3339), e.Repo, e.Branch, e.SessionID, e.Text)
	}
	for _, id := range slices.Sorted(maps.Keys(r.Refused)) {
		fmt.Fprintf(w, "\nnot shown: %s: the audit still finds %s after scrubbing\n", id, strings.Join(r.Refused[id], ", "))
	}
	switch {
	case len(r.Missing) > 0 && r.Unread != nil:
		fmt.Fprintf(w, "\nnot in the part of the vault that could be read: %s\n", strings.Join(r.Missing, ", "))
	case len(r.Missing) > 0:
		fmt.Fprintf(w, "\nnot in the vault: %s (sent before the vault began, more than %d days ago, or on another machine)\n",
			strings.Join(r.Missing, ", "), vault.KeepDays)
	}
}
