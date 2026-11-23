// Package gitinfo answers the hook's questions about the repository it runs in: origin,
// slug, branch, top level, and whether the origin is in scope, under one time budget.
//
// A timed-out lookup returns "" exactly as "no origin" does; only TimedOut tells them
// apart, so a timeout reads as "scope unknown", which is said, never as "out of scope",
// which leaves no trace.
package gitinfo

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/0xbew/livy/internal/oldnames"
	"github.com/0xbew/livy/internal/pytext"
)

// ScopeHost is the host in scope when scope.hosts names none.
const ScopeHost = "github.com"

// Timeouts: one per call (configurable, clamped so a setting cannot push the hook past
// Claude Code's 5 s hook timeout) and one budget for every lookup in a hook together.
const (
	TimeoutEnv     = "LIVY_GIT_TIMEOUT"
	timeoutDefault = 1.2
	timeoutMin     = 0.2
	TimeoutMax     = 3.0
	Budget         = 3 * time.Second
)

// oldTimeoutEnv is the Python tool's name for TimeoutEnv, read when TimeoutEnv is unset; ""
// in a build with none.
var oldTimeoutEnv = oldnames.RecorderEnv("GIT_TIMEOUT")

// CategoryFile marks a folder outside every repo as a declared working folder (one of
// Categories): its events are recorded with repo "unknown".
const CategoryFile = ".livy-category"

// OldCategoryFile is the Python tool's marker, read only where CategoryFile does not exist;
// "" in a build with none.
var OldCategoryFile = oldCategoryFile()

func oldCategoryFile() string {
	if d := oldnames.RecorderDir(); d != "" {
		return d + "-category"
	}
	return ""
}

// Categories are the declared folder roles.
var Categories = []string{"librarian"}

// Git runs git (and ssh -G) for one hook invocation.
type Git struct {
	Stderr io.Writer
	// DefaultTimeout is the per-call timeout in seconds used when the environment sets none;
	// zero means the hook's. A caller off the hook path (backfill) raises it.
	DefaultTimeout float64
	Hosts          []string // hosts whose repositories can be in scope; nil means ScopeHost, empty means none
	deadline       time.Time
	timedOut       bool
	origins        map[string]string
	branches       map[string]string
	tops           map[string]string
}

// New starts a budget of Budget for every lookup made through the returned Git.
func New(stderr io.Writer) *Git {
	return &Git{Stderr: stderr, deadline: time.Now().Add(Budget), origins: map[string]string{},
		branches: map[string]string{}, tops: map[string]string{}}
}

// NewUnbudgeted is New without the shared budget, for a background reader (extract) that
// resolves thousands of folders in one run: a spent budget would read every later folder as
// out of scope. Each call keeps its own timeout.
func NewUnbudgeted(stderr io.Writer) *Git {
	g := New(stderr)
	g.deadline = time.Now().Add(100 * 365 * 24 * time.Hour)
	return g
}

// TimedOut reports whether a lookup since the last reset hit its timeout.
func (g *Git) TimedOut() bool { return g.timedOut }

// BudgetSpent reports whether the lookup budget is used up, so a further lookup would
// answer "" without asking.
func (g *Git) BudgetSpent() bool { return g.remaining() <= 0 }

// MarkTimedOut sets the timeout flag again, for a caller that cleared it to look.
func (g *Git) MarkTimedOut() { g.timedOut = true }

// ResetTimedOut clears the timeout flag.
func (g *Git) ResetTimedOut() { g.timedOut = false }

// Timeout is the per-call timeout in seconds from TimeoutEnv (or the old name while it is
// unset), said aloud when unusable.
func (g *Git) Timeout() float64 {
	def := timeoutDefault
	if g.DefaultTimeout > 0 {
		def = g.DefaultTimeout
	}
	name := TimeoutEnv
	raw, ok := os.LookupEnv(name)
	if !ok && oldTimeoutEnv != "" {
		name = oldTimeoutEnv
		raw, ok = os.LookupEnv(name)
	}
	if !ok {
		return def
	}
	v, err := strconv.ParseFloat(pytext.Strip(raw), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		fmt.Fprintf(g.Stderr, "ledger: %s=%s is not a finite number; using %ss\n", name, pytext.Repr(raw),
			strconv.FormatFloat(def, 'f', -1, 64))
		return def
	}
	return math.Min(math.Max(v, timeoutMin), TimeoutMax)
}

// FormatSeconds is Python's "{:g}" of a timeout.
func FormatSeconds(s float64) string { return strconv.FormatFloat(s, 'g', 6, 64) }

func (g *Git) remaining() time.Duration { return time.Until(g.deadline) }

// run runs name with args; "" on any failure. A timeout sets the flag.
func (g *Git) run(name string, args ...string) string {
	limit := time.Duration(g.Timeout() * float64(time.Second))
	if r := g.remaining(); r < limit {
		limit = r
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = nil // never wait on a credential or pager prompt
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.Output()
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		g.timedOut = true
		return ""
	}
	if err != nil || !utf8.Valid(out) {
		return ""
	}
	return pytext.Strip(string(out))
}

// Run is a git value, or "": never an error and never a hang.
func (g *Git) Run(args ...string) string {
	if g.remaining() <= 0 {
		fmt.Fprintf(g.Stderr, "ledger: git budget spent; skipping git %s (value will be unknown)\n", args[0])
		return ""
	}
	return g.run("git", args...)
}

// answer is Run, or ok false when it timed out: a timeout is not an answer, so a cache
// must not keep it. The flag is left set by a timeout and otherwise as it was.
func (g *Git) answer(args ...string) (string, bool) {
	earlier := g.timedOut
	g.timedOut = false
	found := g.Run(args...)
	if g.timedOut {
		return "", false
	}
	g.timedOut = earlier
	return found, true
}

// Cwd is the process's working directory as the kernel reports it, ignoring $PWD.
func Cwd() (string, bool) {
	d, err := syscall.Getwd()
	return d, err == nil
}

// Origin is remote.origin.url for dir ("" = the working directory), asked once per dir.
// get-url, not config --get: it applies url.<base>.insteadOf rewrites.
func (g *Git) Origin(dir string) string {
	if dir == "" {
		var ok bool
		if dir, ok = Cwd(); !ok {
			return ""
		}
	}
	if u, ok := g.origins[dir]; ok {
		return u
	}
	if u, ok := g.answer("-C", dir, "remote", "get-url", "origin"); ok {
		g.origins[dir] = u
		return u
	}
	return ""
}

// sshAlias is a host token as it may appear in a remote URL; anything else (notably a
// leading "-") is refused before it reaches ssh's argv, where it would be an option.
var sshAlias = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

// sshHostname is what an SSH config alias resolves to (`ssh -G` reads config; no network).
func (g *Git) sshHostname(alias string) string {
	if !sshAlias.MatchString(alias) {
		return ""
	}
	if g.remaining() <= 0 {
		fmt.Fprintln(g.Stderr, "ledger: git budget spent; not resolving the SSH alias")
		return ""
	}
	out := g.run("ssh", "-G", alias)
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		key, value, _ := strings.Cut(sc.Text(), " ")
		if key == "hostname" {
			return strings.ToLower(pytext.Strip(value))
		}
	}
	return ""
}

// InScope is true only for an origin on a host in g.Hosts (github.com when none are set)
// whose owner is in orgs(). orgs is asked only once the URL parses, as it may say why it
// found none.
func (g *Git) InScope(url string, orgs func() []string) bool {
	host, owner, _, ok := RemoteOwnerRepo(url)
	if !ok {
		return false
	}
	found := false
	for _, o := range orgs() {
		if strings.ToLower(owner) == o {
			found = true
		}
	}
	if !found {
		return false
	}
	hosts := map[string]bool{}
	for _, h := range g.Hosts {
		if h = strings.ToLower(pytext.Strip(h)); h != "" {
			hosts[h] = true
		}
	}
	if g.Hosts == nil {
		hosts[ScopeHost] = true
	}
	if hosts[host] {
		return true
	}
	// An SSH alias (git@github-work:org/x.git) is the usual setup with two GitHub accounts.
	isSSH := strings.HasPrefix(url, "ssh://") || !strings.Contains(url, "://")
	return isSSH && hosts[g.sshHostname(host)]
}

// Slug is owner/name from the working directory's origin, else the top level's folder
// name, else "unknown"; it matches the repo CI's capture writes, so the two join.
func (g *Git) Slug() string {
	if _, owner, name, ok := RemoteOwnerRepo(g.Origin("")); ok {
		return owner + "/" + name
	}
	if top := g.Run("rev-parse", "--show-toplevel"); top != "" {
		return filepath.Base(top)
	}
	return "unknown"
}

// Branch is the current branch, asked once per working directory, or "unknown".
func (g *Git) Branch() string {
	b, _ := g.BranchOrWhy()
	return b
}

// BranchOrWhy is Branch, and why it is "unknown" when git ran out of time saying ("" when
// git answered: a folder outside a repo is unknown and not a failure). The why comes once
// per working directory, as the answer is asked once.
func (g *Git) BranchOrWhy() (branch, why string) {
	cwd, ok := Cwd()
	if !ok {
		return "unknown", ""
	}
	if b, ok := g.branches[cwd]; ok {
		return b, ""
	}
	spent := g.BudgetSpent()
	b, answered := g.answer("rev-parse", "--abbrev-ref", "HEAD")
	switch {
	case spent:
		why = fmt.Sprintf("git timed out: its %ss lookup budget was spent before reading the branch", FormatSeconds(Budget.Seconds()))
	case !answered:
		why = fmt.Sprintf("git timed out after %ss reading the branch", FormatSeconds(g.Timeout()))
	}
	if b == "" {
		b = "unknown"
	}
	g.branches[cwd] = b
	return b, why
}

// Toplevel is the working directory's repository root, asked once, or "".
func (g *Git) Toplevel() string {
	cwd, ok := Cwd()
	if !ok {
		return ""
	}
	if t, ok := g.tops[cwd]; ok {
		return t
	}
	t := g.Run("rev-parse", "--show-toplevel")
	g.tops[cwd] = t
	return t
}

// Category is the declared category of dir, or "". Refused inside any git work tree, or
// when git cannot say in time, so a repo's name never rides along on a stray marker.
func (g *Git) Category(dir string) string {
	f, err := os.Open(filepath.Join(dir, CategoryFile))
	if errors.Is(err, fs.ErrNotExist) && OldCategoryFile != "" {
		f, err = os.Open(filepath.Join(dir, OldCategoryFile))
	}
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 64*4)
	n, _ := io.ReadFull(f, buf)
	if !utf8.Valid(buf[:n]) && !utf8.Valid(trimPartial(buf[:n])) {
		return ""
	}
	word := []rune(string(buf[:n]))
	if len(word) > 64 {
		word = word[:64]
	}
	w := pytext.Strip(string(word))
	known := false
	for _, c := range Categories {
		known = known || c == w
	}
	if !known {
		return ""
	}
	if top, ok := g.answer("-C", dir, "rev-parse", "--show-toplevel"); ok && top == "" {
		return w
	}
	return ""
}

// trimPartial drops a multi-byte character cut off by the read limit.
func trimPartial(p []byte) []byte {
	for i := 0; i < 3 && len(p) > 0 && !utf8.Valid(p); i++ {
		p = p[:len(p)-1]
	}
	return p
}

// RemoteOwnerRepo is (host lowercased, owner, repo) from an ssh, scp-style or https
// remote URL, or ok false.
func RemoteOwnerRepo(url string) (host, owner, repo string, ok bool) {
	cleaned := strings.TrimRight(pytext.Strip(url), "/")
	cleaned = strings.TrimSuffix(cleaned, ".git")
	var path string
	if _, rest, found := strings.Cut(cleaned, "://"); found {
		host, path, _ = strings.Cut(rest, "/")
		if i := strings.LastIndex(host, "@"); i >= 0 {
			host = host[i+1:]
		}
		host, _, _ = strings.Cut(host, ":")
	} else if strings.Contains(cleaned, ":") {
		host, path, _ = strings.Cut(cleaned, ":")
		if i := strings.LastIndex(host, "@"); i >= 0 {
			host = host[i+1:]
		}
	} else {
		return "", "", "", false
	}
	var parts []string
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) != 2 || host == "" {
		return "", "", "", false
	}
	return strings.ToLower(host), parts[0], parts[1], true
}
