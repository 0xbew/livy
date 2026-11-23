package why

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/gitinfo"
	"github.com/0xbew/livy/internal/hook"
)

// ghTimeout bounds each gh call: an offline machine must not hold why up.
const ghTimeout = 20 * time.Second

// prCommitsPage is how many of a PR's commits are asked for per page.
const prCommitsPage = 100

// prCommitsListed is the most commits GitHub lists for a pull request, however many it has.
const prCommitsListed = 250

// answeredStatuses are the HTTP statuses that answer about one request (this commit or PR is not
// there); any other failure (expired token, rate limit, server error) would repeat for every
// commit.
var answeredStatuses = []string{"(HTTP 404)", "(HTTP 422)"}

// subjectPR is the "(#123)" GitHub ends a squash commit's subject with.
var subjectPR = regexp.MustCompile(`\(#([0-9]+)\)\s*$`)

// Squash is the pull request a commit with no agent session merged, and that PR's own commits:
// on a main where pull requests are squash-merged (or merged with a merge commit), the commits
// the agent work was recorded against.
type Squash struct {
	PR        int         // 0 when no pull request was found
	Merge     bool        // the commit is a merge commit, not a squash
	Originals []Explained // the PR's commits, oldest first, each with its report
	Problem   string      // why the PR or its commits were not read; "" when they were
}

// ghFunc runs gh with args and returns its stdout.
type ghFunc func(ctx context.Context, args ...string) ([]byte, error)

// ghError is a gh call that failed: what it said, and whether GitHub answered with an HTTP
// status (about this one request) rather than gh failing to ask (not signed in, offline).
type ghError struct {
	msg      string
	answered bool
}

func (e *ghError) Error() string { return e.msg }

// prLookup finds the pull request behind a commit through the GitHub API, read-only. Once gh
// fails to ask at all, off says why and no later commit asks again; off is set from the start
// when no lookup can be made.
type prLookup struct {
	gh   ghFunc
	repo string // owner/name
	host string // passed to gh, so GH_HOST cannot send the request elsewhere; "" leaves gh's default
	off  string
}

// newPRLookup is a lookup for the repository why runs in: off when it is out of scope, has no
// origin on a forge gh can ask, or gh is not installed. It asks git and nothing else.
func newPRLookup() *prLookup {
	if !hook.NewRecorder(io.Discard).InScope() {
		return &prLookup{off: "this repo is not in scope"}
	}
	host, owner, name, ok := gitinfo.RemoteOwnerRepo(gitinfo.New(io.Discard).Origin(""))
	if !ok {
		return &prLookup{off: "the origin names no owner/repo"}
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return &prLookup{off: "gh is not installed"}
	}
	return &prLookup{gh: runGH, repo: owner + "/" + name, host: host}
}

// find is the pull request c merged, by the API or else by the "(#N)" ending subject, and its
// commits explained from events. It never fails: what it could not read is in Problem.
func (l *prLookup) find(ctx context.Context, c Commit, subject string, events []Event) *Squash {
	sq := &Squash{Merge: c.Parents > 1}
	pr, err := l.prOf(ctx, c.SHA)
	if pr == 0 {
		pr = prInSubject(subject)
	}
	sq.PR = pr
	switch {
	case pr == 0 && err != nil:
		sq.Problem = fmt.Sprintf("no pull request found that merged it (%v)", err)
	case pr == 0:
		sq.Problem = "no pull request found that merged it"
	case l.off != "":
		sq.Problem = fmt.Sprintf("PR #%d, named in its subject, not read: %s", pr, l.off)
	default:
		originals, err := l.commitsOf(ctx, pr)
		if err != nil {
			sq.Problem = fmt.Sprintf("the commits of PR #%d could not be read (%v)", pr, err)
			return sq
		}
		sq.Originals = explainOriginals(originals, events)
		if len(originals) >= prCommitsListed {
			sq.Problem = fmt.Sprintf("GitHub lists only the first %d commits of PR #%d", prCommitsListed, pr)
		}
	}
	return sq
}

// pullRef is the part of a pull request GitHub lists for a commit that find reads.
type pullRef struct {
	Number         int    `json:"number"`
	MergeCommitSHA string `json:"merge_commit_sha"`
}

// prOf is the pull request GitHub says was merged as sha, 0 when none was.
func (l *prLookup) prOf(ctx context.Context, sha string) (int, error) {
	out, err := l.call(ctx, "repos/"+l.repo+"/commits/"+sha+"/pulls")
	if err != nil {
		return 0, err
	}
	var prs []pullRef
	if err := json.Unmarshal(out, &prs); err != nil {
		return 0, fmt.Errorf("unreadable answer from GitHub: %w", err)
	}
	for _, p := range prs {
		if p.MergeCommitSHA == sha {
			return p.Number, nil
		}
	}
	return 0, nil
}

// prCommit is the part of a commit GitHub lists for a pull request that find reads.
type prCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message   string `json:"message"`
		Committer struct {
			Date string `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

// commitsOf is the pull request's commits, oldest first. gh --paginate writes one JSON array
// per page, back to back.
func (l *prLookup) commitsOf(ctx context.Context, pr int) ([]prCommit, error) {
	out, err := l.call(ctx, "--paginate", fmt.Sprintf("repos/%s/pulls/%d/commits?per_page=%d", l.repo, pr, prCommitsPage))
	if err != nil {
		return nil, err
	}
	var all []prCommit
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var page []prCommit
		err := dec.Decode(&page)
		if errors.Is(err, io.EOF) {
			return all, nil
		}
		if err != nil {
			return nil, fmt.Errorf("unreadable answer from GitHub: %w", err)
		}
		all = append(all, page...)
	}
}

// call runs one read-only `gh api` request. A failure to ask turns the lookup off, so an
// offline or signed-out machine waits for gh once, not once per commit.
func (l *prLookup) call(ctx context.Context, args ...string) ([]byte, error) {
	if l.off != "" {
		return nil, errors.New(l.off)
	}
	full := []string{"api", "--method", "GET"}
	if l.host != "" {
		full = append(full, "--hostname", l.host)
	}
	out, err := l.gh(ctx, append(full, args...)...)
	if err == nil {
		return out, nil
	}
	var ge *ghError
	if !errors.As(err, &ge) || !ge.answered {
		l.off = err.Error()
	}
	return nil, err
}

// runGH runs gh under ghTimeout, never prompting.
func runGH(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	if ctx.Err() != nil {
		return nil, &ghError{msg: fmt.Sprintf("gh: no answer in %s", ghTimeout)}
	}
	return nil, ghFailure(errb.String(), err)
}

// ghFailure is a failed gh call as a ghError. gh says an HTTP status only in its message
// ("Not Found (HTTP 404)"), so the message is the one way to tell GitHub's answer about a
// request from gh failing to ask.
func ghFailure(stderr string, err error) *ghError {
	msg := strings.TrimSpace(stderr)
	if line, _, _ := strings.Cut(msg, "\n"); line != "" {
		msg = line
	}
	if msg == "" {
		msg = err.Error()
	}
	if !strings.HasPrefix(msg, "gh:") {
		msg = "gh: " + msg
	}
	answered := slices.ContainsFunc(answeredStatuses, func(s string) bool { return strings.Contains(stderr, s) })
	return &ghError{msg: msg, answered: answered}
}

// prInSubject is the PR number a "(#N)" ending subject names, 0 when it names none.
func prInSubject(subject string) int {
	m := subjectPR.FindStringSubmatch(subject)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// explainOriginals explains each of a pull request's commits. One this clone has is read from
// git, with its own history; one it lacks (a deleted branch) from GitHub's answer, with the
// PR's earlier commits as its history.
func explainOriginals(pcs []prCommit, events []Event) []Explained {
	out := make([]Explained, 0, len(pcs))
	var earlier []Commit // newest first
	for _, pc := range pcs {
		c, err := commitFacts(pc.SHA)
		if err != nil {
			c = apiCommit(pc, earlier)
		}
		earlier = append([]Commit{{SHA: c.SHA, Committed: c.Committed, Sessions: c.Sessions}}, earlier...)
		out = append(out, Explained{Commit: c, Report: Explain(c, events)})
	}
	return out
}

// apiCommit is a commit as GitHub lists it: its trailers read by git from the message.
func apiCommit(pc prCommit, ancestors []Commit) Commit {
	c := Commit{SHA: pc.SHA, Ancestors: ancestors}
	if t, ok := parseTS(pc.Commit.Committer.Date); ok {
		c.Committed = t
	}
	out, err := gitInput(pc.Commit.Message, "interpret-trailers", "--parse")
	if err != nil {
		return c
	}
	for line := range strings.SplitSeq(out, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "agent-session":
			c.Sessions = append(c.Sessions, value)
		case "agent-harness":
			c.Harness = cmp.Or(c.Harness, value)
		case "agent-model":
			c.Model = cmp.Or(c.Model, value)
		}
	}
	return c
}

// Describe is what the commit was and how many commits its pull request had: "squash merge of
// PR #N (3 original commits)", or "merge of PR #N (1 original commit)".
func (sq *Squash) Describe() string {
	kind, unit := "squash merge", "original commits"
	if sq.Merge {
		kind = "merge"
	}
	if len(sq.Originals) == 1 {
		unit = "original commit"
	}
	return fmt.Sprintf("%s of PR #%d (%d %s)", kind, sq.PR, len(sq.Originals), unit)
}

// Sessions is the agent sessions behind e: its own, then those behind each commit of the pull
// request it merged, in commit order.
func (e Explained) Sessions() []Session {
	out := slices.Clone(e.Report.Sessions)
	if e.Squash != nil {
		for _, o := range e.Squash.Originals {
			out = append(out, o.Report.Sessions...)
		}
	}
	return out
}
