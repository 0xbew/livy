// Package pr is `livy pr --json`: what went into one pull request's commits (the agent sessions
// behind each, where their time went, and the skills they loaded) in one document, read from the
// keyspace index instead of every line on the machine. The same `why` and `profile` code explains
// the commits, so the answer matches theirs; only what is read differs. With no usable index it
// reads everything, as they do, and says so.
package pr

import (
	"cmp"
	"flag"
	"fmt"
	"io"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/0xbew/livy/internal/emptywin"
	"github.com/0xbew/livy/internal/extract/cli"
	"github.com/0xbew/livy/internal/profile"
	"github.com/0xbew/livy/internal/tooltarget"
	"github.com/0xbew/livy/internal/why"
)

// Schema names the document `livy pr --json` writes. Fields are only ever added; a change that
// removes or retypes one is a new schema value.
const Schema = "livy.pr/1"

const usage = "usage: livy pr --json --repo OWNER/NAME --branch BRANCH [--commits SHA,...] [--top N] [--scan]"

// How a document's lines were read.
const (
	kindIndex = "index"
	kindScan  = "scan"
)

const (
	eventRecorded  = "commit.recorded"
	eventRewritten = "commit.rewritten"
	sourceHook     = "hook"
	eventUsage     = "usage.api_call"
)

// gcPercent is the garbage collector's target while a document is built.
const gcPercent = 400

// defaultTop is how many of the longest spans each window lists, as `livy profile`.
const defaultTop = 5

// Doc is `livy pr --json`.
type Doc struct {
	Schema string `json:"schema"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	// Commits are the commits explained, full shas, in the order given (or recorded).
	Commits []string           `json:"commits"`
	Source  Source             `json:"source"`
	Why     why.JSONReport     `json:"why"`
	Profile profile.JSONReport `json:"profile"`
	// Skills are the skill loads inside the commits' session windows, oldest first.
	Skills []Skill `json:"skills"`
	// SkillsInContext is which skills the commits' sessions had in context, used or not.
	SkillsInContext SkillsInContext `json:"skills_in_context"`
	// Cost is the estimated API cost of the commits' windows at list prices, and their cache use.
	Cost Cost `json:"cost"`
	// EmptyWindows are the commits' session windows with no model call in the ledger, each with why,
	// so a reader says the window's calls, tools and time are unknown and why, never 0.
	EmptyWindows []EmptyWindow `json:"empty_windows"`
}

// EmptyWindow is one session window with no model call line: the commit and session, a fixed
// kind ("not_extracted", "out_of_scope", "no_calls", "no_transcript", "harness" or "unreadable") and the reason
// in words.
type EmptyWindow struct {
	Commit    string `json:"commit"`
	SessionID string `json:"session_id"`
	Kind      string `json:"kind"`
	Reason    string `json:"reason"`
}

// Source says how the document's lines were read: "index", or "scan" (every line on the machine,
// as `livy why` reads them) with the reason the index was not used.
type Source struct {
	Kind    string     `json:"kind"`
	Reason  string     `json:"reason"`
	BuiltAt *time.Time `json:"index_built_at"`
	// Files is how many index files were read.
	Files int `json:"index_files"`
}

// Skill is one skill use inside a window: when, by which session and agent, how it was used, and
// the SKILL.md's hash (never its text) when the hook saw it load. Commits are the commits whose
// windows hold it.
type Skill struct {
	At         time.Time `json:"ts"`
	SessionID  string    `json:"session_id"`
	AgentID    string    `json:"agent_id"`
	Repo       string    `json:"repo"`
	Skill      string    `json:"skill"`
	Scope      string    `json:"scope"`
	Trigger    string    `json:"trigger"`
	SHA256     string    `json:"sha256"`
	TreeSHA256 string    `json:"tree_sha256"`
	Commits    []string  `json:"commits"`
	// Method is how the skill was used: "skill_tool" (the agent called the Skill tool), "read" (it
	// read the skill's SKILL.md with the Read tool) or "slash" (a person typed /name); "" for a
	// trigger this build does not know. Source is the line's writer: "hook" or "transcript".
	Method string `json:"method"`
	Source string `json:"source"`
}

type options struct {
	repo, branch string
	commits      []string
	top          int
	scan         bool
}

// Main runs `livy pr` and returns the exit status: 0 with a document, 1 with a document missing
// something it could not read, 2 on bad arguments or a commit git does not know.
func Main(args []string, stdout, stderr io.Writer) int {
	o, ok := parse(args, stderr)
	if !ok {
		return 2
	}
	// One short-lived run that holds a PR's lines at once: collecting garbage less often is
	// most of a second on a long session's hundred thousand lines.
	debug.SetGCPercent(gcPercent)
	doc, err := build(o)
	if err != nil {
		fmt.Fprintf(stderr, "livy pr: %v\n", err)
		return 2
	}
	if err := why.WriteJSON(stdout, doc); err != nil {
		fmt.Fprintf(stderr, "livy pr: write the document: %v\n", err)
		return 2
	}
	if len(doc.Why.Read.Unread) > 0 || len(doc.Profile.Read.Unread) > 0 {
		return 1
	}
	return 0
}

func parse(args []string, stderr io.Writer) (options, bool) {
	fl := flag.NewFlagSet("livy pr", flag.ContinueOnError)
	fl.SetOutput(stderr)
	asJSON := fl.Bool("json", false, "write the "+Schema+" document (the only output)")
	repo := fl.String("repo", "", "the pull request's repository, OWNER/NAME")
	branch := fl.String("branch", "", "the pull request's head branch")
	commits := fl.String("commits", "", "the pull request's commits, comma-separated (default: those recorded on the branch)")
	top := fl.Int("top", defaultTop, "how many of the longest spans each window lists")
	scan := fl.Bool("scan", false, "read every line on the machine instead of the index, as `livy why` does")
	rest, err := why.ParseAnyOrder(fl, args)
	if err != nil {
		return options{}, false
	}
	if len(rest) > 0 || !*asJSON || *repo == "" || *branch == "" || *top < 0 {
		fmt.Fprintln(stderr, usage)
		return options{}, false
	}
	o := options{repo: *repo, branch: *branch, top: *top, scan: *scan}
	for s := range strings.SplitSeq(*commits, ",") {
		if s = strings.TrimSpace(s); s != "" {
			o.commits = append(o.commits, s)
		}
	}
	return o, true
}

// build is the document for o.
func build(o options) (Doc, error) {
	var ld loaded
	shas := o.commits
	if len(shas) == 0 {
		found, err := recordedOnBranch(o)
		if err != nil {
			return Doc{}, err
		}
		shas = found
	}
	facts := make([]why.Commit, len(shas))
	errs := make([]error, len(shas))
	each(len(shas), func(i int) { facts[i], errs[i] = why.CommitFacts(shas[i]) })
	for i, err := range errs {
		if err != nil {
			return Doc{}, fmt.Errorf("%s: %w", why.Short(shas[i]), err)
		}
	}
	ok := false
	if !o.scan {
		ld, ok = fromIndex(dataDir(), facts)
	}
	if !ok {
		reason := ld.source.Reason
		if o.scan {
			reason = "--scan"
		}
		ld = fromScan(reason)
	}
	return assemble(o, facts, ld), nil
}

// assemble explains each commit from what was read and puts the document together.
func assemble(o options, facts []why.Commit, ld loaded) Doc {
	explained := make([]why.Explained, len(facts))
	doc := Doc{Schema: Schema, Repo: o.repo, Branch: o.branch, Commits: []string{}, Source: ld.source, Skills: []Skill{},
		EmptyWindows: []EmptyWindow{}}
	each(len(facts), func(i int) { explained[i] = why.Explained{Commit: facts[i], Report: why.Explain(facts[i], ld.events)} })
	for _, c := range facts {
		doc.Commits = append(doc.Commits, c.SHA)
	}
	doc.Why = why.JSONReport{Schema: why.JSONSchema, Commits: make([]why.JSONCommit, 0, len(explained)), Read: ld.read}
	for _, e := range explained {
		doc.Why.Commits = append(doc.Why.Commits, why.ToJSON(e))
	}
	targets, _ := tooltarget.Read(dataDir(), profile.SessionsOf(explained))
	doc.Profile = profile.Report(explained, ld.events, ld.spans, o.top, targets)
	doc.EmptyWindows = emptyWindows(explained, emptywin.New(dataDir(), cli.Roots(nil)))
	doc.Profile.Read = ld.spanRd
	doc.Profile.Read.Summary = ld.read.Summary
	doc.Profile.Read.Unread = append(append([]string{}, ld.read.Unread...), ld.spanRd.Unread...)
	doc.Skills = skillsIn(explained, ld.events)
	doc.SkillsInContext = skillsInContext(explained, ld.events, doc.Skills)
	doc.Cost = costOf(explained, ld.events)
	return doc
}

// workers bounds how many commits are read from git, or explained, at once.
const workers = 8

// each runs f(0..n-1), up to workers at a time, and waits for all of them.
func each(n int, f func(int)) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, workers)
	for i := range n {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			f(i)
		})
	}
	wg.Wait()
}

// skillsIn is the skill uses inside the commits' windows, each once: by the window's session, after its
// start (when it has one) and up to its end.
func skillsIn(explained []why.Explained, events []why.Event) []Skill {
	out := []Skill{}
	for _, e := range why.SkillLoads(events) {
		t, ok := why.ParseTS(e.TS)
		if !ok {
			continue
		}
		if commits := windowsHolding(explained, e.SessionID, t); len(commits) > 0 {
			out = append(out, Skill{At: t, SessionID: e.SessionID, AgentID: e.AgentID, Repo: e.Repo, Skill: e.Skill,
				Scope: e.Scope, Trigger: e.Trigger, SHA256: e.SHA256, TreeSHA256: e.TreeSHA256, Commits: commits,
				Method: why.SkillMethod(e.Trigger), Source: cmp.Or(e.Source, sourceHook)})
		}
	}
	slices.SortStableFunc(out, func(a, b Skill) int { return a.At.Compare(b.At) })
	return out
}

// windowsHolding is the commits with a window of session that holds t: after its start, when
// it has one, and up to its end.
func windowsHolding(explained []why.Explained, session string, t time.Time) []string {
	var out []string
	for _, ex := range explained {
		for _, s := range ex.Report.Sessions {
			in := s.ID == session && (s.From.IsZero() || t.After(s.From)) && !t.After(s.To)
			if in && !slices.Contains(out, ex.Commit.SHA) {
				out = append(out, ex.Commit.SHA)
			}
		}
	}
	return out
}

// emptyWindows is each window of the commits with no model call, with why it is empty.
func emptyWindows(explained []why.Explained, x *emptywin.Explainer) []EmptyWindow {
	out := []EmptyWindow{}
	for _, e := range explained {
		for _, s := range e.Report.Sessions {
			if len(s.Calls) == 0 {
				f := x.Explain(s)
				out = append(out, EmptyWindow{Commit: e.Commit.SHA, SessionID: s.ID, Kind: f.Kind, Reason: f.Reason})
			}
		}
	}
	return out
}
