// Package extract holds what every transcript extractor shares: which repo a recorded
// working folder belongs to (scope, per call), where a tool call worked (target fields),
// timestamps, names, the ledger writer and the state file. The harness-specific readers
// live in claudecode/ and codex/.
package extract

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/gitinfo"
	"github.com/0xbew/livy/internal/hook"
)

// Unknown is the repo of a declared folder outside every repo, and a branch nobody can name.
const Unknown = "unknown"

// Target vias, as tool_target writes them.
const (
	ViaSession = "session"
)

// Scope answers "which in-scope repo is this folder in" for many folders in one run, asking
// git once per folder. A folder whose origin git could not read in time is remembered, and
// every read of it counts in TimeoutReads, so a reader can leave the file holding it unread
// for the next run instead of dropping its records as out of scope.
type Scope struct {
	Git          *gitinfo.Git
	orgs         func() []string
	home         string
	repos        map[string]string // folder -> slug, "unknown" (declared folder), or "" (out of scope)
	timedOutCwds map[string]bool
	TimedOutDirs map[string]bool
	TimeoutReads int

	roots   map[string]string
	slugs   map[string]string
	moves   map[string]*history
	current map[string]string
	// Problems are path-free reasons a target could not be resolved, with counts.
	Problems map[string]int
}

// NewScope reads the in-scope orgs afresh on each check, as the hook does.
func NewScope(stderr io.Writer) *Scope {
	home, _ := os.UserHomeDir()
	return &Scope{Git: gitinfo.NewUnbudgeted(stderr), orgs: func() []string { return hook.Orgs(stderr) }, home: home,
		repos: map[string]string{}, timedOutCwds: map[string]bool{}, TimedOutDirs: map[string]bool{},
		roots: map[string]string{}, slugs: map[string]string{}, moves: map[string]*history{},
		current: map[string]string{}, Problems: map[string]int{}}
}

// RepoFor is owner/name for an in-scope folder, "unknown" for a declared folder outside
// every repo, else "". A folder that no longer exists is read as its nearest existing
// parent, so a removed worktree is still its repo's.
func (s *Scope) RepoFor(cwd string) string {
	if !strings.HasPrefix(cwd, "/") {
		return ""
	}
	if r, ok := s.repos[cwd]; ok {
		if s.timedOutCwds[cwd] {
			s.TimeoutReads++
		}
		return r
	}
	probe := cwd
	for probe != "" && probe != "/" && !isDir(probe) {
		probe = filepath.Dir(probe)
	}
	result := ""
	if probe != "" && probe != "/" && probe != s.home {
		var slow bool
		result, slow = s.lookup(probe)
		if slow {
			s.TimedOutDirs[probe] = true
			s.timedOutCwds[cwd] = true
			s.TimeoutReads++
		}
	}
	s.repos[cwd] = result
	return result
}

// lookup is the repo a folder belongs to: owner/name when its origin is in scope, Unknown
// when it is some other git repo, "" otherwise; slow when git timed out deciding.
func (s *Scope) lookup(probe string) (result string, slow bool) {
	s.Git.ResetTimedOut()
	url := s.Git.Origin(probe)
	ours := url != "" && s.Git.InScope(url, s.orgs)
	slow = !ours && s.Git.TimedOut()
	if ours {
		if _, owner, name, ok := gitinfo.RemoteOwnerRepo(url); ok {
			result = owner + "/" + name
		}
	}
	if result == "" && !slow {
		s.Git.ResetTimedOut()
		if s.Git.Category(probe) != "" {
			result = Unknown
		} else {
			slow = s.Git.TimedOut()
		}
	}
	return result, slow
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Named is a folder a tool call named: its via and absolute folder ("" when only the shell
// knows it).
type Named struct{ Via, Folder string }

// NamedBy is the folder one tool call names, or nil.
func NamedBy(tool string, input any, cwd string) *Named {
	via, folder, ok := hook.NamedDirectory(tool, input, cwd)
	if !ok {
		return nil
	}
	return &Named{via, folder}
}

// Placed is where one call is scoped: its repo, and whether its session branch still
// applies (false when it was placed by a folder it named, or the folder is a declared one).
type Placed struct {
	Repo       string
	KeepBranch bool
}

// Place scopes one call by its own folder, else by the first folder it named that is in
// scope; ok false when it is out of scope.
func (s *Scope) Place(cwd string, haveCwd bool, named []*Named) (Placed, bool) {
	repo := ""
	if haveCwd {
		repo = s.RepoFor(cwd)
	}
	if repo == Unknown {
		return Placed{repo, false}, true
	}
	if repo != "" {
		return Placed{repo, true}, true
	}
	for _, n := range named {
		if n == nil || n.Folder == "" {
			continue
		}
		if r := s.RepoFor(n.Folder); r != "" {
			return Placed{r, false}, true
		}
	}
	return Placed{}, false
}

// TargetFields are target_via, target_repo and target_branch for a call, given the folders
// its tool calls named; when is the call's instant (the branch checked out then).
func (s *Scope) TargetFields(named []*Named, sessionCwd string, sessionRepo, sessionBranch string, when *time.Time) map[string]any {
	branch := sessionBranch
	if branch == "" {
		branch = Unknown
	}
	var cands []*Named
	for _, n := range named {
		if n != nil {
			cands = append(cands, n)
		}
	}
	if len(cands) == 0 {
		return map[string]any{"target_via": ViaSession, "target_repo": sessionRepo, "target_branch": branch}
	}
	for _, c := range cands {
		if c.Folder == "" {
			continue
		}
		if repo, br, ok := s.place(c.Folder, sessionCwd, branch, when); ok {
			return map[string]any{"target_via": c.Via, "target_repo": repo, "target_branch": br}
		}
	}
	return map[string]any{"target_via": cands[0].Via}
}

func (s *Scope) root(folder string) string {
	if r, ok := s.roots[folder]; ok {
		return r
	}
	found := ""
	for probe := folder; ; {
		if _, err := os.Stat(filepath.Join(probe, ".git")); err == nil {
			found = hook.Realpath(probe)
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	s.roots[folder] = found
	return found
}

func (s *Scope) slug(root string) string {
	if v, ok := s.slugs[root]; ok {
		return v
	}
	url := s.Git.Origin(root)
	v := ""
	if url != "" && s.Git.InScope(url, s.orgs) {
		if _, owner, name, ok := gitinfo.RemoteOwnerRepo(url); ok {
			v = owner + "/" + name
		}
	}
	s.slugs[root] = v
	return v
}

func (s *Scope) place(folder, sessionCwd, sessionBranch string, when *time.Time) (string, string, bool) {
	top := s.root(folder)
	if top == "" {
		return "", "", false
	}
	slug := s.slug(top)
	if slug == "" {
		return "", "", false
	}
	if _, err := os.Stat(folder); err != nil {
		// A worktree removed since: the repo is still known, which branch it held is not.
		return slug, Unknown, true
	}
	if strings.HasPrefix(sessionCwd, "/") && s.root(sessionCwd) == top {
		return slug, sessionBranch, true
	}
	branch, decided := "", false
	if when != nil {
		h, ok := s.moves[top]
		if !ok {
			log := s.Git.Run("-C", top, "reflog", "show", "--date=unix", "--format=%gd%x09%gs", "HEAD")
			// Never empty for a checkout (its creation is logged), so empty means git failed.
			if log != "" {
				h = parseReflog(log)
			}
			s.moves[top] = h
		}
		if h == nil {
			s.Problems["target: an in-scope folder's history could not be read; recorded as unknown"]++
			return slug, Unknown, true
		}
		if float64(when.Unix()) < float64(h.oldest) {
			// Git expires old entries, so a call older than every one left may predate a move.
			return slug, Unknown, true
		}
		branch, decided = branchAt(h.moves, when.Unix())
	}
	if !decided {
		cur, ok := s.current[top]
		if !ok {
			cur = s.Git.Run("-C", top, "rev-parse", "--abbrev-ref", "HEAD")
			s.current[top] = cur
		}
		branch = cur
	}
	if branch == "" {
		s.Problems["target: an in-scope folder's branch could not be read; recorded as unknown"]++
		branch = Unknown
	}
	return slug, branch, true
}

// BranchAt is the branch the repo holding folder had checked out at when, from its reflog,
// or ok false when the folder is not in an in-scope repo.
func (s *Scope) BranchAt(folder string, when *time.Time) (repo, branch string, ok bool) {
	return s.place(folder, "", "", when)
}

type move struct {
	at            int64
	before, after string
}

type history struct {
	oldest int64
	moves  []move
}

var (
	reflogLine = regexp.MustCompile(`^HEAD@\{(\d+)\}(?:\t(.*))?$`)
	checkoutRe = regexp.MustCompile(`^checkout: moving from (\S+) to (\S+)$`)
	returnRe   = regexp.MustCompile(`^rebase(?: -i)? \((?:finish|abort)\): returning to (\S+)$`)
	renameRe   = regexp.MustCompile(`^Branch: renamed (\S+) to (\S+)$`)
	detachedRe = regexp.MustCompile(`^(?:[0-9a-f]{7,40}|origin/.*|.*[~^:].*)$`)
)

// refName is a checkout destination as a branch name, or HEAD when it is not a branch: a
// commit, a remote-tracking ref, any refs/ other than heads, or a revision expression.
func refName(ref string) string {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	if detachedRe.MatchString(ref) || (strings.HasPrefix(ref, "refs/") && !strings.HasPrefix(ref, "refs/heads/")) {
		return "HEAD"
	}
	return ref
}

func parseReflog(text string) *history {
	var moves []move
	var oldest *int64
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		e := reflogLine.FindStringSubmatch(line)
		if e == nil {
			continue
		}
		at, err := strconv.ParseInt(e[1], 10, 64)
		if err != nil {
			continue
		}
		if oldest == nil || at < *oldest {
			v := at
			oldest = &v
		}
		subject := e[2]
		if m := checkoutRe.FindStringSubmatch(subject); m != nil {
			moves = append(moves, move{at, refName(m[1]), refName(m[2])})
			continue
		}
		if m := renameRe.FindStringSubmatch(subject); m != nil {
			moves = append(moves, move{at, refName(m[1]), refName(m[2])})
			continue
		}
		if m := returnRe.FindStringSubmatch(subject); m != nil {
			moves = append(moves, move{at, refName(m[1]), refName(m[1])})
		}
	}
	if oldest == nil {
		return nil
	}
	for i, j := 0, len(moves)-1; i < j; i, j = i+1, j-1 {
		moves[i], moves[j] = moves[j], moves[i]
	}
	return &history{*oldest, moves}
}

// branchAt is the branch checked out at second, or ok false when HEAD never moved. A move
// stamped in the call's own second is the call's doing, so it counts as after it.
func branchAt(moves []move, second int64) (string, bool) {
	checked, set := "", false
	for _, m := range moves {
		if m.at >= second {
			if set {
				return checked, true
			}
			return m.before, true
		}
		checked, set = m.after, true
	}
	return checked, set
}

// Orgs is the in-scope orgs, read afresh.
func (s *Scope) Orgs() []string { return s.orgs() }
