package profile

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/ledgerfiles"
	"github.com/0xbew/livy/internal/relay"
	"github.com/0xbew/livy/internal/why"
)

const usage = "usage: livy profile [--top N] [--all] [--json] [--data DIR]... <sha|A..B>"

// defaultTop is how many of the longest spans each session lists.
const defaultTop = 5

// options are the command's arguments.
type options struct {
	rev  string
	data []string
	top  int
	all  bool
	json bool
}

// Main runs `livy profile [--top N] [--all] [--data DIR]... <sha|A..B>` and returns the exit
// status: 0 with a report, 1 with a report missing a folder it could not read, 2 on bad
// arguments or a revision git does not know.
func Main(args []string, stdout, stderr io.Writer) int {
	o, ok := parse(args, stderr)
	if !ok {
		return 2
	}
	r, err := why.Explore(o.rev, o.data)
	if err != nil {
		fmt.Fprintf(stderr, "livy profile: %v\n", err)
		return 2
	}
	spans, read := loadSpans(o.data)
	if o.json {
		b := &jsonBuilder{spans: spans, events: r.Events, top: o.top, agentTypes: agentTypesOf(r.Events)}
		return writeJSON(b, r, read, stdout, stderr)
	}
	rep := report{w: stdout, spans: spans, events: r.Events, top: o.top, all: o.all}
	for _, e := range r.Commits {
		rep.commit(e, "")
	}
	rep.summary(read)
	fmt.Fprint(stdout, r.Sources)
	if len(r.Unread) > 0 || len(read.missing) > 0 {
		return 1
	}
	return 0
}

// parse reads the arguments; false (with the reason or usage on stderr) when they are wrong.
func parse(args []string, stderr io.Writer) (options, bool) {
	fl := flag.NewFlagSet("livy profile", flag.ContinueOnError)
	fl.SetOutput(stderr)
	fl.Usage = func() {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintln(stderr, "Where each commit's time went: wall time from the first prompt split into agent time and time")
		fmt.Fprintln(stderr, "waiting on the human, the span tree per turn, the longest spans, and rework signals. Text-free.")
		fl.PrintDefaults()
	}
	var data multi
	top := fl.Int("top", defaultTop, "how many of the longest model calls, tools and subagents each session lists")
	all := fl.Bool("all", false, "print every span in the tree (by default a turn lists its first "+fmt.Sprint(childrenShown)+")")
	fl.Var(&data, "data", "also read ledger lines and OTEL spans from this folder (*.jsonl, *.jsonl.gz), e.g. a copy downloaded from the bucket (repeatable)")
	asJSON := fl.Bool("json", false, "write one JSON document (schema "+JSONSchema+") instead of text: the time split, turns, waits, rework and longest spans per session")
	rest, err := why.ParseAnyOrder(fl, args)
	if err != nil {
		return options{}, false
	}
	if len(rest) != 1 || strings.HasPrefix(rest[0], "-") || *top < 0 {
		fmt.Fprintln(stderr, usage)
		return options{}, false
	}
	return options{rev: rest[0], data: data, top: *top, all: *all, json: *asJSON}, true
}

// writeJSON writes the range's profile as one JSON document and returns the exit status, as
// Main does for text.
func writeJSON(b *jsonBuilder, r why.Range, read spanSources, stdout, stderr io.Writer) int {
	out := JSONReport{Schema: JSONSchema, Commits: make([]JSONCommit, 0, len(r.Commits))}
	for _, e := range r.Commits {
		out.Commits = append(out.Commits, b.commit(e))
	}
	out.Totals = b.totals
	out.Read = JSONRead{Summary: r.Sources, Spans: read.spans, SpanFiles: read.files, Bad: read.bad,
		Unread: append(append([]string{}, r.Unread...), read.missing...)}
	if err := why.WriteJSON(stdout, out); err != nil {
		fmt.Fprintf(stderr, "livy profile: write the report: %v\n", err)
		return 2
	}
	if len(out.Read.Unread) > 0 {
		return 1
	}
	return 0
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// spanSources says what loadSpans read.
type spanSources struct {
	files, spans, bad int
	missing           []string // folders that exist, or were asked for, but could not be read
}

// loadSpans reads every span still on this machine: the receiver's otel stream (its live file,
// segments and the shipped ones emit keeps), a local sink's objects, and each --data folder.
// Lines of other signals, and ledger lines, are skipped.
func loadSpans(data []string) ([]Span, spanSources) {
	var src spanSources
	var spans []Span
	read := func(p string) {
		s, bad, err := ReadSpansFile(p)
		src.files++
		src.bad += bad
		if err != nil {
			src.bad++
		}
		spans = append(spans, s...)
	}
	stream := relay.OtelStreamDir(hook.DataDir())
	n, err := ledgerfiles.Walk(stream, read)
	src.bad += n
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		src.missing = append(src.missing, "otel stream "+stream)
	}
	dirs := append([]string(nil), data...)
	if root := ledgerfiles.LocalSink(); root != "" {
		dirs = append(dirs, root)
	}
	for _, d := range dirs {
		n, err := ledgerfiles.Walk(d, read)
		src.bad += n
		if err != nil {
			src.missing = append(src.missing, d)
		}
	}
	src.spans = len(spans)
	return spans, src
}
