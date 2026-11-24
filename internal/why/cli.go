package why

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/0xbew/livy/internal/emit"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/ledgerfiles"
	"github.com/0xbew/livy/internal/relay"
)

// gitTimeout bounds each git call; why runs by hand, outside any tool loop.
const gitTimeout = 30 * time.Second

// ancestorsRead is how far back a commit's first-parent line is read for the previous commit
// of a session that has no commit.recorded.
const ancestorsRead = 200

// idsShown is how many prompt ids a session prints before eliding the middle, unless --all.
const idsShown = 20

// maxLine is the longest ledger line read.
const maxLine = 16 << 20

// sources says where events were read from, for the report's last line.
type sources struct {
	files   map[string]int // source label -> files read
	events  int
	dupes   int // lines read twice (a --data copy of the sink, a segment both kept and shipped)
	oldest  time.Time
	bad     int      // unreadable files or lines
	missing []string // the ledger or folders that exist, or were asked for, but could not be read
	seen    map[[sha256.Size]byte]bool
}

// Main runs `livy why [--data DIR]... <sha|range>` and returns the exit status: 0 with a
// report, 1 with a report that is missing a folder it could not read, 2 on bad arguments or a
// revision git does not know.
func Main(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("livy why", flag.ContinueOnError)
	fl.SetOutput(stderr)
	var data multi
	all := fl.Bool("all", false, "list every prompt id (by default a long list keeps its first and last ten)")
	perPrompt := fl.Bool("per-prompt", false, "also one line per prompt: its tools, how they ended, its permission mode, calls and tokens")
	fl.Var(&data, "data", "also read ledger lines from this folder (*.jsonl, *.jsonl.gz), e.g. a copy downloaded from the bucket (repeatable)")
	asJSON := fl.Bool("json", false, "write one JSON document (schema "+JSONSchema+") instead of text: every prompt id and the per-prompt lines")
	rest, err := parseAnyOrder(fl, args)
	if err != nil {
		return 2
	}
	if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
		fmt.Fprintln(stderr, "usage: livy why [--all] [--per-prompt] [--json] [--data DIR]... <sha|A..B>")
		return 2
	}
	shas, err := revisions(rest[0])
	if err != nil {
		fmt.Fprintf(stderr, "livy why: %v\n", err)
		return 2
	}
	events, src := load(data)
	prs := sync.OnceValue(newPRLookup)
	var explained []Explained
	for _, sha := range shas {
		e, err := explainCommit(context.Background(), sha, events, prs)
		if err != nil {
			fmt.Fprintf(stderr, "livy why: %s: %v\n", short(sha), err)
			return 2
		}
		if *asJSON {
			explained = append(explained, e)
			continue
		}
		write(stdout, e, view{all: *all, perPrompt: *perPrompt})
	}
	if *asJSON {
		if err := WriteJSON(stdout, newJSONReport(explained, src)); err != nil {
			fmt.Fprintf(stderr, "livy why: write the report: %v\n", err)
			return 2
		}
	} else {
		writeSources(stdout, src)
	}
	if len(src.missing) > 0 {
		return 1
	}
	return 0
}

// parseAnyOrder parses fl's flags wherever they stand among args, and returns the other
// arguments in order: the flag package alone stops at the first one that is not a flag. A "--"
// ends the flags; everything after it is an argument.
func parseAnyOrder(fl *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for {
		if err := fl.Parse(args); err != nil {
			return nil, fmt.Errorf("parse flags: %w", err)
		}
		left := fl.Args()
		if read := len(args) - len(left); read > 0 && args[read-1] == "--" {
			return append(rest, left...), nil
		}
		if len(left) == 0 {
			return rest, nil
		}
		rest = append(rest, left[0])
		args = left[1:]
	}
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// revisions is the commits a revision names: one commit, or a range's commits oldest first.
func revisions(rev string) ([]string, error) {
	if strings.Contains(rev, "..") {
		out, err := git("rev-list", "--reverse", "--end-of-options", rev, "--")
		if err != nil {
			return nil, err
		}
		shas := strings.Fields(out)
		if len(shas) == 0 {
			return nil, emptyRange(rev)
		}
		return shas, nil
	}
	sha, err := commitOf(rev)
	if err != nil {
		return nil, err
	}
	return []string{sha}, nil
}

func commitOf(rev string) (string, error) {
	sha, err := git("rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	return strings.TrimSpace(sha), err
}

// emptyRange is the error for an A..B that names no commits, saying why when A and B are both
// commits: B is A, or B is behind A (every commit B reaches, A reaches too).
func emptyRange(rev string) error {
	a, b, _ := strings.Cut(rev, "..")
	if a == "" || b == "" || strings.HasPrefix(b, ".") {
		return fmt.Errorf("%s names no commits", rev)
	}
	shaA, errA := commitOf(a)
	shaB, errB := commitOf(b)
	switch {
	case errA != nil || errB != nil:
		return fmt.Errorf("%s names no commits", rev)
	case shaA == shaB:
		return fmt.Errorf("%s names no commits: %s and %s are the same commit", rev, a, b)
	}
	return fmt.Errorf("%s names no commits: %s is behind %s; did you mean %s..%s?", rev, b, a, b, a)
}

// Each commit's facts come in one field per NUL and end with a record separator; trailers with
// several values are comma-joined.
const commitFormat = "%H%x00%cI%x00%(trailers:key=Agent-Session,valueonly,separator=%x2C)%x00" +
	"%(trailers:key=Agent-Harness,valueonly,separator=%x2C)%x00%(trailers:key=Agent-Model,valueonly,separator=%x2C)%x1e"

// commitFields is how many fields commitFormat writes.
const commitFields = 5

// logArgs pin what git log prints whatever the user's config says.
var logArgs = []string{"-c", "log.showSignature=false", "log", "--no-show-signature", "--format=" + commitFormat}

func commitFacts(sha string) (Commit, error) {
	out, err := git(append(slices.Clone(logArgs), "-1", sha, "--")...)
	if err != nil {
		return Commit{}, err
	}
	cs := parseLog(out)
	if len(cs) != 1 {
		return Commit{}, fmt.Errorf("git log gave an unreadable record")
	}
	c := cs[0]
	parents, err := git("rev-list", "--parents", "-n1", sha, "--")
	if err != nil {
		return Commit{}, err
	}
	c.Parents = max(len(strings.Fields(parents))-1, 0)
	if c.Parents == 0 {
		return c, nil // a root commit: nothing earlier to find
	}
	anc, err := git(append(slices.Clone(logArgs), "--first-parent", fmt.Sprintf("-n%d", ancestorsRead), sha+"^", "--")...)
	if err != nil {
		c.AncestorProblem = err.Error()
		return c, nil
	}
	c.Ancestors = parseLog(anc)
	return c, nil
}

// parseLog reads git log output in commitFormat, one commit per record.
func parseLog(out string) []Commit {
	var cs []Commit
	for rec := range strings.SplitSeq(out, "\x1e") {
		if strings.TrimSpace(rec) == "" {
			continue
		}
		if c, ok := parseCommit(rec); ok {
			cs = append(cs, c)
		}
	}
	return cs
}

func parseCommit(rec string) (Commit, bool) {
	f := strings.Split(strings.Trim(rec, "\n"), "\x00")
	if len(f) != commitFields {
		return Commit{}, false
	}
	t, err := time.Parse(time.RFC3339, f[1])
	if err != nil {
		return Commit{}, false
	}
	c := Commit{SHA: f[0], Committed: t.UTC(), Harness: firstOf(f[3]), Model: firstOf(f[4])}
	for s := range strings.SplitSeq(f[2], ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.Sessions = append(c.Sessions, s)
		}
	}
	return c, true
}

func firstOf(list string) string {
	v, _, _ := strings.Cut(list, ",")
	return strings.TrimSpace(v)
}

// explainCommit is sha's report. A commit no agent session is found behind is looked up as a
// merge of a pull request (prs, made on first use), whose own commits are explained instead.
func explainCommit(ctx context.Context, sha string, events []Event, prs func() *prLookup) (Explained, error) {
	c, err := commitFacts(sha)
	if err != nil {
		return Explained{}, err
	}
	e := Explained{Commit: c, Report: Explain(c, events)}
	if len(e.Report.Sessions) > 0 {
		return e, nil
	}
	subject, err := git("log", "-1", "--no-show-signature", "--format=%s", sha, "--")
	if err != nil {
		subject = ""
	}
	l := prs()
	if l.off != "" {
		e.Squash = &Squash{Problem: "pull request not looked up: " + l.off}
		return e, nil
	}
	e.Squash = l.find(ctx, c, strings.TrimSpace(subject), events)
	return e, nil
}

// git runs one git command, each under its own gitTimeout.
func git(args ...string) (string, error) { return gitInput("", args...) }

// gitInput runs one git command with input on its stdin, under its own gitTimeout.
func gitInput(input string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdin = strings.NewReader(input)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s: no answer in %s", strings.Join(args, " "), gitTimeout)
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(errb.String()))
	}
	return string(out), nil
}

// load reads every ledger line still on this machine: the ledger, its spill files, the
// segments not yet shipped, the shipped ones emit keeps, a local sink's objects, and each
// --data folder.
func load(data []string) ([]Event, *sources) {
	src := &sources{files: map[string]int{}, seen: map[[sha256.Size]byte]bool{}}
	var events []Event
	dir := hook.DataDir()
	read := func(label, path string) error {
		evs, err := readFile(src, label, path)
		events = append(events, evs...)
		return err
	}
	readCounted := func(label string) func(string) {
		return func(p string) {
			if read(label, p) != nil {
				src.bad++
			}
		}
	}
	if err := read("ledger", filepath.Join(dir, ledger.FileName)); err != nil {
		src.missing = append(src.missing, "ledger")
	}
	// These folders exist only while they hold something.
	for _, d := range []struct{ label, root string }{{"spill", filepath.Join(dir, ledger.SpillDir)},
		{"unshipped segments", filepath.Join(dir, emit.SegmentsDir)},
		{"uploaded segments kept", filepath.Join(dir, emit.UploadedDir)},
		{"tool decisions kept", filepath.Join(dir, relay.DecisionsDir)}} {
		n, err := ledgerfiles.Walk(d.root, readCounted(d.label))
		src.bad += n
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			src.missing = append(src.missing, d.label)
		}
	}
	required := slices.Clone(data)
	if root := ledgerfiles.LocalSink(); root != "" {
		required = append(required, root)
	}
	for _, d := range required {
		label := "--data " + d
		if !slices.Contains(data, d) {
			label = "local sink"
		}
		n, err := ledgerfiles.Walk(d, readCounted(label))
		src.bad += n
		if err != nil {
			src.missing = append(src.missing, label)
		}
	}
	return events, src
}

// readFile reads one file's events, counting unreadable lines in src; the error is why a file
// that exists could not be opened.
func readFile(src *sources, label, path string) ([]Event, error) {
	fh, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = fh.Close() }() // opened read-only: a close error loses nothing
	var r io.Reader = fh
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(fh)
		if err != nil {
			src.bad++
			return nil, nil
		}
		r = gz
	}
	src.files[label]++
	var out []Event
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		sum := sha256.Sum256(sc.Bytes())
		if src.seen[sum] {
			src.dupes++
			continue
		}
		src.seen[sum] = true
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			src.bad++
			continue
		}
		if t, ok := parseTS(e.TS); ok && (src.oldest.IsZero() || t.Before(src.oldest)) {
			src.oldest = t
		}
		src.events++
		out = append(out, e)
	}
	if sc.Err() != nil {
		src.bad++
	}
	return out, nil
}

// view is how much of a report is printed.
type view struct{ all, perPrompt bool }

func write(w io.Writer, e Explained, v view) {
	c, r := e.Commit, e.Report
	fmt.Fprintf(w, "commit %s  committed %s\n", short(c.SHA), c.Committed.Format(time.RFC3339))
	trailers := "none"
	if len(c.Sessions) > 0 {
		trailers = "Agent-Session " + strings.Join(c.Sessions, ", ")
		if c.Harness != "" {
			trailers += "; Agent-Harness " + c.Harness
		}
		if c.Model != "" {
			trailers += "; Agent-Model " + c.Model
		}
	}
	fmt.Fprintf(w, "  trailers: %s\n", trailers)
	if c.AncestorProblem != "" {
		fmt.Fprintf(w, "  earlier commits unread (%s): a session's window may start too early\n", c.AncestorProblem)
	}
	if len(r.Sessions) == 0 {
		fmt.Fprintln(w, "  no agent session: no Agent-Session trailer and no commit.recorded line for it here")
	}
	for _, s := range r.Sessions {
		writeSession(w, s, v)
	}
	if e.Squash != nil {
		writeSquash(w, e.Squash, v)
	}
	fmt.Fprintln(w)
}

// writeSquash says which pull request a commit merged and reports each of its commits,
// indented under it, or says why it could not.
func writeSquash(w io.Writer, sq *Squash, v view) {
	if len(sq.Originals) == 0 {
		fmt.Fprintf(w, "  %s\n", cmp.Or(sq.Problem, "no pull request found that merged it"))
		return
	}
	if sq.Problem != "" {
		fmt.Fprintf(w, "  %s\n", sq.Problem)
	}
	fmt.Fprintf(w, "  %s; the agent work behind each:\n", sq.Describe())
	for _, o := range sq.Originals {
		var b strings.Builder
		write(&b, o, v)
		for line := range strings.Lines(strings.TrimRight(b.String(), "\n")) {
			fmt.Fprint(w, "    "+line)
		}
		fmt.Fprintln(w)
	}
}

func writeSession(w io.Writer, s Session, v view) {
	from := "the session's start"
	if !s.From.IsZero() {
		from = s.From.Format(time.RFC3339)
	}
	ticket := s.Ticket
	if ticket == "" {
		ticket = "not recorded"
	}
	harness := s.Harness
	if harness == "" {
		harness = "harness unknown"
	}
	fmt.Fprintf(w, "  session %s (%s), ticket %s\n", s.ID, harness, ticket)
	kind, _ := BasisKind(s.Basis)
	fmt.Fprintf(w, "    window: after %s, up to %s (basis %s: %s)\n", from, s.To.Format(time.RFC3339), kind, s.Basis)
	fmt.Fprintf(w, "    prompts (%d): %s\n", len(s.Prompts), orNone(strings.Join(shown(s.Prompts, v.all), ", ")))
	if len(s.AgentBriefs) > 0 {
		fmt.Fprintf(w, "    agent briefs (%d): %s\n", len(s.AgentBriefs), strings.Join(s.AgentBriefs, ", "))
	}
	fmt.Fprintf(w, "    tools: %s\n", orNone(counts(s.Tools)))
	models := make([]string, 0, len(s.Models))
	for m := range s.Models {
		models = append(models, m)
	}
	sort.Strings(models)
	if len(models) == 0 {
		fmt.Fprintln(w, "    models: none recorded")
	}
	for _, m := range models {
		u, name := s.Models[m], m
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(w, "    model %s: %d calls, tokens in %d, out %d, cache read %d, cache write %d\n",
			name, u.Calls, u.Input, u.Output, u.CacheRead, u.CacheWrite)
	}
	if v.perPrompt {
		writePerPrompt(w, s.PerPrompt)
	}
}

// writePerPrompt prints one line per prompt in the window.
func writePerPrompt(w io.Writer, ps []PromptSummary) {
	if len(ps) == 0 {
		fmt.Fprintln(w, "    per prompt: no prompt lines in this window (extraction has not read it yet?)")
		return
	}
	for _, p := range ps {
		mode := p.Mode
		if mode == "" {
			mode = "mode not recorded"
		}
		sent := "sent earlier"
		if !p.At.IsZero() {
			sent = p.At.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "    %s  %s (%s): tools %s; ended %s; approvals %s; %s\n", p.ID, sent, mode,
			orNone(counts(p.Tools)), orNone(counts(p.Results)), orNone(counts(p.Approvals)), modelsLine(p.Models))
	}
}

// modelsLine is a prompt's calls and tokens per model on one line.
func modelsLine(m map[string]Usage) string {
	if len(m) == 0 {
		return "no calls recorded"
	}
	names := slices.Sorted(maps.Keys(m))
	parts := make([]string, 0, len(names))
	for _, n := range names {
		u := m[n]
		parts = append(parts, fmt.Sprintf("%s %d calls, in %d, out %d, cache read %d, cache write %d",
			n, u.Calls, u.Input, u.Output, u.CacheRead, u.CacheWrite))
	}
	return strings.Join(parts, "; ")
}

// shown is ids, or past idsShown its first and last half with the number left out between.
func shown(ids []string, all bool) []string {
	if all || len(ids) <= idsShown {
		return ids
	}
	half := idsShown / 2
	out := append([]string(nil), ids[:half]...)
	out = append(out, fmt.Sprintf("... %d more (--all lists them) ...", len(ids)-idsShown))
	return append(out, ids[len(ids)-half:]...)
}

func counts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] || (m[keys[i]] == m[keys[j]] && keys[i] < keys[j]) })
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func orNone(s string) string {
	if s == "" {
		return "none recorded"
	}
	return s
}

func writeSources(w io.Writer, src *sources) {
	labels := make([]string, 0, len(src.files))
	for l := range src.files {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, fmt.Sprintf("%s (%d)", l, src.files[l]))
	}
	oldest := "none"
	if !src.oldest.IsZero() {
		oldest = src.oldest.Format(time.RFC3339)
	}
	fmt.Fprintf(w, "read: %d events from %s; oldest %s", src.events, orNone(strings.Join(parts, ", ")), oldest)
	if src.dupes > 0 {
		fmt.Fprintf(w, "; %d duplicate lines counted once", src.dupes)
	}
	if src.bad > 0 {
		fmt.Fprintf(w, "; %d unreadable files or lines", src.bad)
	}
	for _, d := range src.missing {
		fmt.Fprintf(w, "\n%s could not be read", d)
	}
	fmt.Fprintf(w, "\nnot read: segments uploaded longer ago than %s keeps them; pass --data with a downloaded copy\n", emit.KeepUploadedKey)
}

// Behind is one commit and the prompts why finds behind it.
type Behind struct {
	SHA     string   // the full SHA
	Prompts []string // prompt ids, in report order
	Unread  []string // folders (or the ledger) that exist but could not be read: Prompts may lack theirs
}

// CommitPrompts is the commit rev names and every prompt id why finds behind it, agent-written
// briefs included: what `livy show --commit` reveals. A range is refused.
func CommitPrompts(rev string) (Behind, error) {
	if strings.Contains(rev, "..") {
		return Behind{}, errors.New("--commit takes one commit, not a range")
	}
	shas, err := revisions(rev)
	if err != nil {
		return Behind{}, err
	}
	c, err := commitFacts(shas[0])
	if err != nil {
		return Behind{}, err
	}
	events, src := load(nil)
	var ids []string
	for _, s := range Explain(c, events).Sessions {
		for _, id := range slices.Concat(s.Prompts, s.AgentBriefs) {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	return Behind{SHA: c.SHA, Prompts: ids, Unread: src.missing}, nil
}

// Explained is one commit and what why says about it.
type Explained struct {
	Commit Commit
	Report Report
	// Squash is the pull request a commit with no agent session merged, and its commits; nil
	// when the commit has a session, so nothing was looked up.
	Squash *Squash
}

// ExplainRange is every commit rev names, oldest first, each with its report, and the lines
// saying what was read. data folders are read as --data reads them.
func ExplainRange(rev string, data []string) ([]Explained, string, error) {
	r, err := Explore(rev, data)
	if err != nil {
		return nil, "", err
	}
	return r.Commits, r.Sources, nil
}

// Range is what why reads for a revision: its commits explained, the ledger events they were
// explained from, and the lines saying what was read.
type Range struct {
	Commits []Explained
	Events  []Event
	Sources string
	// Unread are the folders (or the ledger) that exist but could not be read.
	Unread []string
}

// Explore is ExplainRange with the events it read, for a command that reads more from them.
func Explore(rev string, data []string) (Range, error) {
	shas, err := revisions(rev)
	if err != nil {
		return Range{}, err
	}
	events, src := load(data)
	prs := sync.OnceValue(newPRLookup)
	out := make([]Explained, 0, len(shas))
	for _, sha := range shas {
		e, err := explainCommit(context.Background(), sha, events, prs)
		if err != nil {
			return Range{}, fmt.Errorf("%s: %w", short(sha), err)
		}
		out = append(out, e)
	}
	var b strings.Builder
	writeSources(&b, src)
	return Range{Commits: out, Events: events, Sources: b.String(), Unread: src.missing}, nil
}

// Git runs one git command in the working directory, under why's timeout.
func Git(args ...string) (string, error) { return git(args...) }

// GitInput runs one git command in the working directory with input on its stdin, under why's timeout.
func GitInput(input string, args ...string) (string, error) { return gitInput(input, args...) }

// ParseAnyOrder parses fl's flags wherever they stand among args, as why does, and returns
// the other arguments in order.
func ParseAnyOrder(fl *flag.FlagSet, args []string) ([]string, error) { return parseAnyOrder(fl, args) }

// ParseTS is a ledger timestamp as UTC; false when it is not RFC 3339.
func ParseTS(s string) (time.Time, bool) { return parseTS(s) }

// Aliases is sha and every SHA it was rewritten from (amend, rebase), newest first.
func Aliases(sha string, events []Event) []string { return aliasesOf(sha, events) }

// Short is a SHA cut to the length why prints.
func Short(sha string) string { return short(sha) }

// OrNone is s, or "none recorded" for an empty one, as why prints it.
func OrNone(s string) string { return orNone(s) }

// OrUnknown is s, or "unknown" for an empty one.
func OrUnknown(s string) string { return orUnknown(s) }

// Counts is a count map as why prints it: largest first, then by name.
func Counts(m map[string]int) string { return counts(m) }
