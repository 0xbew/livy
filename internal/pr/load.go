package pr

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/keyspace"
	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/ledgerfiles"
	"github.com/0xbew/livy/internal/profile"
	"github.com/0xbew/livy/internal/relay"
	"github.com/0xbew/livy/internal/why"
)

// Source labels, as the report's read section names them.
const (
	labelIndex   = "index"
	labelLive    = "ledger"
	labelSpill   = "spill"
	labelPending = "index pending"
)

// loaded is the ledger events and spans one report reads, and where they came from.
type loaded struct {
	events []why.Event
	read   why.JSONRead
	spans  []profile.Span
	spanRd profile.JSONRead
	source Source
}

// extraFile is one file's lines read beside the index.
type extraFile struct {
	label string
	lines []why.Decoded
}

// closure reads, from one index generation, the files a set of commits' reports read: their
// commit buckets, the sessions behind them, the buckets of every commit those sessions made, the
// branches the commits were recorded on, and the sessions' spans. Each file is read once.
type closure struct {
	x   *keyspace.Index
	col *why.Collector
	// read is each index file's lines, decoded, by path; collect adds them in a fixed order.
	read map[string][]why.Decoded
	// extra are the live ledger's, spill's and pending files' lines, collected after the index's.
	extra []extraFile
	// aliases are the commits' shas and every sha they were rewritten from; buckets every sha
	// whose bucket was read; sessions the sessions behind the commits.
	aliases, buckets, sessions map[string]bool
	branches                   map[[2]string]bool
	spans                      []profile.Span
	spanRd                     profile.JSONRead
	err                        error
}

func newClosure(x *keyspace.Index) *closure {
	return &closure{x: x, col: why.NewCollector(), read: map[string][]why.Decoded{}, aliases: map[string]bool{},
		buckets: map[string]bool{}, sessions: map[string]bool{}, branches: map[[2]string]bool{}}
}

// file reads one index file's ledger lines, once.
func (c *closure) file(rel string) {
	if _, done := c.read[rel]; done {
		return
	}
	lines, err := c.x.Lines(rel)
	if err != nil {
		c.err = errors.Join(c.err, err)
	}
	c.read[rel] = why.DecodeLines(lines)
}

// collect adds the files read to the collector: sessions first, then commit buckets, then
// branches, each kind in order of its key. A session's lines thus keep the order they were written
// in, which is what `why` goes by between lines of one instant (commits made in one second).
func (c *closure) collect() {
	rank := func(rel string) int {
		switch {
		case strings.HasPrefix(rel, keyspace.SessionDir):
			return 0
		case strings.HasPrefix(rel, keyspace.CommitDir):
			return 1
		}
		return 2
	}
	order := slices.SortedFunc(maps.Keys(c.read), func(a, b string) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		// By the key's own name, not its shard folder.
		return strings.Compare(strings.TrimSuffix(filepath.Base(a), ".jsonl"), strings.TrimSuffix(filepath.Base(b), ".jsonl"))
	})
	for _, rel := range order {
		c.col.Add(labelIndex, c.read[rel])
	}
	for _, x := range c.extra {
		c.col.Add(x.label, x.lines)
	}
}

// commits reads everything the reports of commits read, to a fixed point.
func (c *closure) commits(commits []why.Commit) {
	for _, cm := range commits {
		c.aliases[cm.SHA] = true
		for _, s := range cm.Sessions {
			c.sessions[s] = true
		}
	}
	for {
		before := len(c.read)
		c.readBuckets()
		for _, s := range sorted(c.sessions) {
			c.file(keyspace.SessionFile(s))
		}
		c.learn(commits)
		if len(c.read) == before {
			break
		}
	}
	for _, b := range slices.SortedFunc(maps.Keys(c.branches), func(x, y [2]string) int {
		return strings.Compare(x[0]+"\x00"+x[1], y[0]+"\x00"+y[1])
	}) {
		c.file(keyspace.BranchFile(b[0], b[1]))
	}
}

// readBuckets reads the bucket of every alias and every sha a session made.
func (c *closure) readBuckets() {
	for sha := range c.aliases {
		c.buckets[sha] = true
	}
	for _, sha := range sorted(c.buckets) {
		c.file(keyspace.CommitBucket(sha))
	}
}

// learn takes new keys from the events read so far: the shas a commit was rewritten from, the
// session and branch that recorded it, the commits the sessions made, and the ancestors their
// trailers name. It looks at every event each time: a line read early (a session's rewrite) can
// match a sha learned later.
func (c *closure) learn(commits []why.Commit) {
	events := c.events()
	c.learnAliases(events)
	for _, e := range events {
		if e.Event != eventRecorded {
			continue
		}
		if c.aliases[e.CommitSHA] {
			c.recordedBehind(e)
		}
		if c.sessions[e.SessionID] && e.CommitSHA != "" {
			c.buckets[e.CommitSHA] = true
		}
	}
	for _, cm := range commits {
		for _, a := range cm.Ancestors {
			if slices.ContainsFunc(a.Sessions, func(s string) bool { return c.sessions[s] }) {
				c.buckets[a.SHA] = true
			}
		}
	}
}

// events is every event read so far, in no particular order.
func (c *closure) events() []why.Event {
	var out []why.Event
	add := func(lines []why.Decoded) {
		for _, d := range lines {
			if e, ok := d.Event(); ok {
				out = append(out, e)
			}
		}
	}
	for _, lines := range c.read {
		add(lines)
	}
	for _, x := range c.extra {
		add(x.lines)
	}
	return out
}

// learnAliases adds every sha the aliases were rewritten from, through any number of rewrites.
func (c *closure) learnAliases(events []why.Event) {
	for grown := true; grown; {
		grown = false
		for _, e := range events {
			if e.Event == eventRewritten && c.aliases[e.CommitSHA] && e.OldCommitSHA != "" && !c.aliases[e.OldCommitSHA] {
				c.aliases[e.OldCommitSHA], grown = true, true
			}
		}
	}
}

// recordedBehind takes the session and branch of a commit.recorded line for one of the commits.
func (c *closure) recordedBehind(e why.Event) {
	if e.SessionID != "" {
		c.sessions[e.SessionID] = true
	}
	if e.Repo != "" && e.Branch != "" {
		c.branches[[2]string{e.Repo, e.Branch}] = true
	}
}

// readSpans reads the sessions' spans, and for each of their traces the spans other sessions
// and no session name: a child session inside a parent's trace, a Codex turn's children.
func (c *closure) readSpans() {
	seenFile := map[string]bool{}
	read := func(rel string) []profile.Span {
		if seenFile[rel] {
			return nil
		}
		seenFile[rel] = true
		lines, err := c.x.Lines(rel)
		if err != nil {
			c.err = errors.Join(c.err, err)
		}
		c.spanRd.SpanFiles++
		var out []profile.Span
		for _, l := range lines {
			s, err := keyspace.DecodeSpan(l)
			if err != nil {
				c.spanRd.Bad++
				continue
			}
			out = append(out, profile.Span(s))
		}
		return out
	}
	traces := map[string]bool{}
	for _, s := range sorted(c.sessions) {
		for _, sp := range read(keyspace.SpanFile(s)) {
			c.spans = append(c.spans, sp)
			traces[sp.Trace] = true
		}
	}
	for _, t := range sorted(traces) {
		c.spans = append(c.spans, read(keyspace.OrphanBucket(t))...)
		c.spans = append(c.spans, c.otherSessions(t, traces, read)...)
	}
}

// otherSessions is the spans of the sessions other than the closure's that trace t names.
func (c *closure) otherSessions(t string, traces map[string]bool, read func(string) []profile.Span) []profile.Span {
	lines, err := c.x.Lines(keyspace.TraceBucket(t))
	if err != nil {
		c.err = errors.Join(c.err, err)
	}
	var out []profile.Span
	for _, l := range lines {
		trace, session, err := keyspace.DecodeTracePair(l)
		if err != nil || trace != t || c.sessions[session] {
			continue
		}
		for _, sp := range read(keyspace.SpanFile(session)) {
			if traces[sp.Trace] {
				out = append(out, sp)
			}
		}
	}
	return out
}

// liveLedger reads what a writer may not have indexed yet, before the closure looks for keys, so
// a commit line only these hold still leads to its session: the live ledger and its spill, and
// the index's pending lines. Lines the index also holds count once.
func (c *closure) liveLedger(dataDir string) {
	add := func(label string) func(string) {
		return func(p string) {
			lines, err := keyspace.ReadLines(p)
			if err != nil {
				c.err = errors.Join(c.err, err)
			}
			if lines != nil {
				c.extra = append(c.extra, extraFile{label: label, lines: why.DecodeLines(lines)})
			}
		}
	}
	add(labelLive)(filepath.Join(dataDir, ledger.FileName))
	c.walk(filepath.Join(dataDir, ledger.SpillDir), add(labelSpill))
	pending, err := keyspace.PendingLedger(dataDir)
	c.err = errors.Join(c.err, err)
	for _, p := range pending {
		add(labelPending)(p)
	}
}

// liveSpans adds the spans a writer may not have indexed yet: the live otel stream, its spill and
// the index's pending exports. profile counts a span read twice once.
func (c *closure) liveSpans(dataDir string) {
	stream := relay.OtelStreamDir(dataDir)
	otelPending, err := keyspace.PendingOtel(dataDir)
	c.err = errors.Join(c.err, err)
	files := append([]string{filepath.Join(stream, ledger.FileName)}, otelPending...)
	c.walk(filepath.Join(stream, ledger.SpillDir), func(p string) { files = append(files, p) })
	for _, p := range files {
		spans, bad, err := profile.ReadSpansFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		c.err = errors.Join(c.err, err)
		c.spanRd.SpanFiles++
		c.spanRd.Bad += bad
		c.spans = append(c.spans, spans...)
	}
}

// walk calls f on each ledger file under root; a folder that is not there has none, and one
// that cannot be listed is an error.
func (c *closure) walk(root string, f func(string)) {
	n, err := ledgerfiles.Walk(root, f)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.err = errors.Join(c.err, err)
	}
	if n > 0 {
		c.err = errors.Join(c.err, fmt.Errorf("%d folders under %s could not be listed", n, root))
	}
}


// fromIndex reads commits' events and spans from dataDir's index. A rebuild that swaps the
// generation mid-read makes it read once more; ok is false when there is no index to read, or
// it kept changing, with the reason in the source.
func fromIndex(dataDir string, commits []why.Commit) (loaded, bool) {
	var reason string
	for range 2 {
		x, err := keyspace.Open(dataDir)
		if err != nil {
			return loaded{source: Source{Kind: kindScan, Reason: err.Error()}}, false
		}
		c := newClosure(x)
		c.liveLedger(dataDir)
		c.commits(commits)
		c.collect()
		c.readSpans()
		c.liveSpans(dataDir)
		if x.Changed() {
			reason = "the index was rebuilt twice while it was read"
			continue
		}
		read := c.col.Read()
		if c.err != nil {
			read.Unread = append(read.Unread, "index: "+c.err.Error())
		}
		for _, u := range x.Manifest.Unread {
			read.Unread = append(read.Unread, "index built without "+u)
		}
		c.spanRd.Spans = len(c.spans)
		built := x.Manifest.BuiltAt
		return loaded{events: c.col.Events(), read: read, spans: c.spans, spanRd: c.spanRd,
			source: Source{Kind: kindIndex, BuiltAt: &built, Files: len(c.read)}}, true
	}
	return loaded{source: Source{Kind: kindScan, Reason: reason}}, false
}

// fromScan reads every ledger line and span on this machine, as `livy why` and `livy profile` do.
func fromScan(reason string) loaded {
	events, read := why.LoadLocal()
	spans, spanRd := profile.LoadLocalSpans()
	return loaded{events: events, read: read, spans: spans, spanRd: spanRd, source: Source{Kind: kindScan, Reason: reason}}
}

// branchCommits is the commits recorded on repo's branch in events, each as the commit it was
// last rewritten into, in the order recorded.
func branchCommits(events []why.Event, repo, branch string) []string {
	next := map[string]string{}
	for _, e := range events {
		if e.Event == eventRewritten && e.OldCommitSHA != "" && e.CommitSHA != "" {
			next[e.OldCommitSHA] = e.CommitSHA
		}
	}
	var out []string
	for _, e := range events {
		if e.Event != eventRecorded || e.Branch != branch || !strings.EqualFold(e.Repo, repo) || e.CommitSHA == "" {
			continue
		}
		sha := e.CommitSHA
		for hops := 0; next[sha] != "" && hops < len(next); hops++ {
			sha = next[sha]
		}
		if !slices.Contains(out, sha) {
			out = append(out, sha)
		}
	}
	return out
}

// sorted is a set's members in order, so the files of a closure are read in the same order
// each run: lines of one instant keep the order they are read in.
func sorted(set map[string]bool) []string { return slices.Sorted(maps.Keys(set)) }

// dataDir is the data folder the hooks write.
func dataDir() string { return hook.DataDir() }
