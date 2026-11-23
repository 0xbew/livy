package incident

import (
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/prompts"
	"github.com/0xbew/livy/internal/show"
	"github.com/0xbew/livy/internal/why"
)

const usage = "usage: livy incident <id> --range A..B [--data DIR]... [--out FILE] [--excerpts]"

var (
	// idPattern is an incident id: a name, since it is printed into the pack's title.
	idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	// revPattern is one end of --range: a SHA, tag, branch or HEAD~n, never an option.
	revPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/~^@-]{0,199}$`)
	// namePattern is what a value from a trailer or the ledger must look like to be printed:
	// ids and names only, so free text a commit message carries never reaches the pack.
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/+\[\]-]{0,127}$`)
)

// notAName stands in for a value that is not an id or a name.
const notAName = "(not a name)"

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// Main runs `livy incident` and returns the exit status: 0 with a pack, 1 when the pack could
// not be written, 2 on bad arguments or a range git cannot list.
func Main(args []string, stdout, stderr io.Writer) int {
	a, code := parseArgs(args, stderr)
	if code != 0 {
		return code
	}
	if a.excerpts {
		if err := excerptsReady(a); err != nil {
			fmt.Fprintf(stderr, "livy incident: %v; no pack written\n", err)
			return 1
		}
	}
	commits, sources, err := why.ExplainRange(a.rng, a.data)
	if err != nil {
		fmt.Fprintf(stderr, "livy incident: %v\n", err)
		return 2
	}
	var section string
	if a.excerpts {
		if section, err = excerpts(commits, stderr); err != nil {
			fmt.Fprintf(stderr, "livy incident: %v; no pack written\n", err)
			return 1
		}
	}
	return deliver(render(a, clock.Now(), commits, sources, section), a.out, len(commits), stdout, stderr)
}

// excerptsReady refuses --excerpts before anything is revealed: with no denylist, or with an
// --out file that exists, which would refuse the pack after the reveals were logged.
func excerptsReady(a incidentArgs) error {
	if names, why := prompts.LoadDenylist(); names == nil {
		return fmt.Errorf("--excerpts: %w (%s)", show.ErrNoDenylist, why)
	}
	if a.out != "" {
		if _, err := os.Lstat(a.out); err == nil {
			return fmt.Errorf("--out %s exists; the pack is never written over a file", a.out)
		}
	}
	return nil
}

// incidentArgs are Main's arguments.
type incidentArgs struct {
	id, rng, out string
	data         multi
	excerpts     bool
}

// parseArgs reads `<id> --range A..B [--data DIR]... [--out FILE] [--excerpts]`; a non-zero code is the exit
// status for a bad one.
func parseArgs(args []string, stderr io.Writer) (incidentArgs, int) {
	var a incidentArgs
	if len(args) == 0 || strings.HasPrefix(args[0], "-") || !idPattern.MatchString(args[0]) {
		fmt.Fprintln(stderr, usage+"\n  the id is letters, digits, '.', '_' or '-', at most 64")
		return a, 2
	}
	a.id = args[0]
	fl := flag.NewFlagSet("livy incident", flag.ContinueOnError)
	fl.SetOutput(stderr)
	fl.StringVar(&a.rng, "range", "", "the deploy range, A..B: the commits after the last good deploy up to the bad one")
	fl.StringVar(&a.out, "out", "", "write the pack to this file (owner-only) instead of stdout")
	fl.Var(&a.data, "data", "also read ledger lines from this folder, as `livy why --data` does (repeatable)")
	fl.BoolVar(&a.excerpts, "excerpts", false, "also include each prompt's kept text, scrubbed and logged as `livy show` does")
	if err := fl.Parse(args[1:]); err != nil {
		return a, 2
	}
	if fl.NArg() != 0 || !isRange(a.rng) {
		fmt.Fprintln(stderr, usage+"\n  --range is two commits, A..B (not A...B)")
		return a, 2
	}
	return a, 0
}

// deliver writes the pack to stdout, or to a new file out; it returns the exit status.
func deliver(pack, out string, commits int, stdout, stderr io.Writer) int {
	if out == "" {
		if _, err := io.WriteString(stdout, pack); err != nil {
			fmt.Fprintf(stderr, "livy incident: the pack could not be written (%v)\n", err)
			return 1
		}
		return 0
	}
	if err := writeNew(out, pack); err != nil {
		fmt.Fprintf(stderr, "livy incident: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "livy incident: wrote %s (%d commits)\n", out, commits)
	return 0
}

// isRange is whether rng is A..B with two plain revisions.
func isRange(rng string) bool {
	a, b, ok := strings.Cut(rng, "..")
	return ok && revPattern.MatchString(a) && revPattern.MatchString(b)
}

// writeNew writes the pack to a file that must not exist yet, owner-only: an existing file, or a
// link, is never overwritten.
func writeNew(path, pack string) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("--out: %w", err)
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("--out: %w", cerr)
		}
	}()
	if _, err := f.WriteString(pack); err != nil {
		return fmt.Errorf("--out: %w", err)
	}
	return nil
}

// name is v when it is an id or a name, else notAName.
func name(v string) string {
	if namePattern.MatchString(v) {
		return v
	}
	return notAName
}

// nameAll is each value through name, each once, in the order given.
func nameAll(vs []string) []string {
	var out []string
	for _, v := range vs {
		if n := name(v); !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// names is a count map with every key passed through name.
func names(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[name(k)] += v
	}
	return out
}

// render is the pack as Markdown.
func render(a incidentArgs, at time.Time, commits []why.Explained, sources, excerpts string) string {
	id, rng := a.id, a.rng
	reports := make([]why.Report, len(commits))
	notes := map[string]string{} // short SHA -> why its pull request was not read
	for i, c := range commits {
		reports[i] = why.Report{SHA: c.Commit.SHA, Sessions: c.Sessions()}
		if note := squashNote(c); note != "" {
			notes[why.Short(c.Commit.SHA)] = note
		}
	}
	t := total(reports)
	var b strings.Builder
	fmt.Fprintf(&b, "# Incident %s: agent work behind `%s`\n\n", id, rng)
	content := "Text-free: prompt ids, tool and model names and counts, never prompt text. If the review needs " +
		"what was asked, the prompt's author runs `livy show --prompt <id>`, which shows it scrubbed and logs the reveal."
	if excerpts != "" {
		content = "Scrubbed prompt text is in Excerpts (asked for with --excerpts); everything else is ids, names and counts."
	}
	fmt.Fprintf(&b, "Generated %s by `livy incident` from this machine's ledger. %s\n\n", at.UTC().Format(time.RFC3339), content)
	b.WriteString("## At a glance\n\n| Commits | With an agent session | Sessions | Prompts | Tickets |\n|---|---|---|---|---|\n")
	fmt.Fprintf(&b, "| %d | %d | %d | %d | %s |\n\n", t.Commits, t.AgentCommits, len(t.Sessions), t.Prompts,
		cell(why.OrNone(strings.Join(nameAll(t.Tickets), ", "))))
	writePartial(&b, commits)
	writeCommitTable(&b, commits)
	writeSessions(&b, commits)
	if len(t.NoAgent) > 0 {
		b.WriteString("## Commits with no agent session found\n\nNo `Agent-Session` trailer and no `commit.recorded` line " +
			"on this machine. Ask the author, or run this on their machine.\n\n")
		for _, s := range t.NoAgent {
			if note, ok := notes[s]; ok {
				fmt.Fprintf(&b, "- `%s`: %s\n", s, cell(note))
				continue
			}
			fmt.Fprintf(&b, "- `%s`\n", s)
		}
		b.WriteString("\n")
	}
	b.WriteString(excerpts)
	b.WriteString("## What was read\n\n```\n" + strings.ReplaceAll(strings.TrimRight(sources, "\n"), "`", "'") + "\n```\n")
	return b.String()
}

// writePartial says which commits' pull requests were read only in part. A commit with no
// session found says it in its own section; one with sessions has none, so it is said here.
func writePartial(b *strings.Builder, commits []why.Explained) {
	var lines []string
	for _, c := range commits {
		if sq := c.Squash; sq != nil && sq.Problem != "" && len(c.Sessions()) > 0 {
			lines = append(lines, fmt.Sprintf("- `%s`: %s\n", why.Short(c.Commit.SHA), cell(sq.Problem)))
		}
	}
	if len(lines) == 0 {
		return
	}
	b.WriteString("**These totals are partial:** the work behind some commits was not all read.\n\n")
	b.WriteString(strings.Join(lines, "") + "\n")
}

func writeCommitTable(b *strings.Builder, commits []why.Explained) {
	b.WriteString("## Commits, in range order\n\n| Commit | Committed (UTC) | Sessions | Prompts | Tools | Ticket | Agent-Model |\n" +
		"|---|---|---|---|---|---|---|\n")
	for _, c := range commits {
		var sessions, tickets []string
		prompts, tools := 0, map[string]int{}
		for _, s := range c.Sessions() {
			if n := name(s.ID) + " (" + name(why.OrUnknown(s.Harness)) + ")"; !slices.Contains(sessions, n) {
				sessions = append(sessions, n)
			}
			if t := name(s.Ticket); s.Ticket != "" && !slices.Contains(tickets, t) {
				tickets = append(tickets, t)
			}
			prompts += len(s.Prompts)
			for k, v := range names(s.Tools) {
				tools[k] += v
			}
		}
		fmt.Fprintf(b, "| %s | %s | %s | %d | %s | %s | %s |\n", commitCell(c),
			c.Commit.Committed.UTC().Format(time.RFC3339), cell(why.OrNone(strings.Join(sessions, ", "))), prompts,
			cell(why.OrNone(why.Counts(tools))), cell(why.OrNone(strings.Join(tickets, ", "))), cell(nameOrNone(c.Commit.Model)))
	}
	b.WriteString("\n")
}

func writeSessions(b *strings.Builder, commits []why.Explained) {
	b.WriteString("## Agent work per commit\n\nEach session's work since its previous commit on the same branch. " +
		"A squash or merge commit with no session of its own lists the work behind each commit of its pull request.\n\n")
	for _, c := range commits {
		if len(c.Sessions()) == 0 {
			continue
		}
		if sq := c.Squash; sq != nil && len(sq.Originals) > 0 {
			fmt.Fprintf(b, "### `%s`: %s\n\n", why.Short(c.Commit.SHA), sq.Describe())
			if sq.Problem != "" {
				fmt.Fprintf(b, "Partial: %s; the work behind the rest is not listed.\n\n", cell(sq.Problem))
			}
			for _, o := range sq.Originals {
				fmt.Fprintf(b, "- commit `%s`\n", why.Short(o.Commit.SHA))
				writeSessionList(b, o.Report.Sessions, "  ")
			}
			b.WriteString("\n")
			continue
		}
		fmt.Fprintf(b, "### `%s`\n\n", why.Short(c.Commit.SHA))
		writeSessionList(b, c.Report.Sessions, "")
		b.WriteString("\n")
	}
}

// writeSessionList is one bullet per session, each line after indent.
func writeSessionList(b *strings.Builder, sessions []why.Session, indent string) {
	if len(sessions) == 0 {
		fmt.Fprintf(b, "%s- no agent session found\n", indent)
	}
	for _, s := range sessions {
		from := "the session's start"
		if !s.From.IsZero() {
			from = s.From.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(b, "%s- session `%s` (%s), ticket %s\n", indent, name(s.ID), name(why.OrUnknown(s.Harness)), nameOrNone(s.Ticket))
		fmt.Fprintf(b, "%s  - window: after %s, up to %s (start: %s)\n", indent, from, s.To.UTC().Format(time.RFC3339), cell(s.Basis))
		fmt.Fprintf(b, "%s  - prompts (%d): %s\n", indent, len(s.Prompts), why.OrNone(ticks(s.Prompts)))
		if len(s.AgentBriefs) > 0 {
			fmt.Fprintf(b, "%s  - agent briefs (%d): %s\n", indent, len(s.AgentBriefs), ticks(s.AgentBriefs))
		}
		fmt.Fprintf(b, "%s  - tools: %s\n", indent, why.OrNone(why.Counts(names(s.Tools))))
		for _, m := range slices.Sorted(maps.Keys(s.Models)) {
			u := s.Models[m]
			fmt.Fprintf(b, "%s  - model %s: %d calls, tokens in %d, out %d, cache read %d, cache write %d\n",
				indent, name(why.OrUnknown(m)), u.Calls, u.Input, u.Output, u.CacheRead, u.CacheWrite)
		}
	}
}

// squashNote is what the pack says beside a commit with no session found: why its pull request
// was not read, or that it was and none of its commits has one; "" when nothing was looked up.
func squashNote(c why.Explained) string {
	sq := c.Squash
	switch {
	case sq == nil:
		return ""
	case sq.Problem != "":
		return sq.Problem
	case len(sq.Originals) > 0 && len(c.Sessions()) == 0:
		return sq.Describe() + ": no agent session found behind any of them"
	}
	return ""
}

// commitCell is a commit's table cell: its short SHA, and the pull request it merged when its
// work was found through that.
func commitCell(c why.Explained) string {
	cellText := "`" + why.Short(c.Commit.SHA) + "`"
	if sq := c.Squash; sq != nil && len(sq.Originals) > 0 {
		cellText += ": " + sq.Describe()
	}
	return cellText
}

// cell keeps a value inside one Markdown table cell.
func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ", "\r", " ").Replace(s)
}

func ticks(ids []string) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = "`" + name(id) + "`"
	}
	return strings.Join(out, ", ")
}

// nameOrNone is v as name prints it, or "none recorded" for an empty one.
func nameOrNone(v string) string {
	if v == "" {
		return why.OrNone("")
	}
	return name(v)
}
