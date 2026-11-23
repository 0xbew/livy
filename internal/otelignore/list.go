// Package otelignore is the otel.ignore list: projects whose Claude Code and Codex OTEL
// exports Livy's receiver drops, so nothing from them is stored or shipped.
//
// An entry names a project the way its owner already knows it: a folder ("/abs/path" or
// "~/path", a glob allowed in any segment), which covers that folder and everything under
// it, or a repository as "owner/repo" (a glob allowed: "me/*"), matched against the origin
// of the repository the session works in, on any host.
//
// A session is placed by its working folder: Claude Code's header helper sends it on every
// export (CwdHeader); a Codex export names only its conversation, whose rollout records the
// folder. With a non-empty list, a record no project can be told for is dropped too: the
// README promises an ignored project leaves no trace, and a record that might be one cannot
// keep that promise.
package otelignore

import (
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/0xbew/livy/internal/config"
	"github.com/0xbew/livy/internal/gitinfo"
)

// Key is the setting: a list of entries, in the managed file or the user config.
const Key = "otel.ignore"

// Verdict is what the list says about one session's folder.
type Verdict int

// Verdicts. Unknown is a folder the list cannot judge (gone, or its origin not learned in
// time); the receiver drops it like an ignored one.
const (
	Unknown Verdict = iota
	Keep
	Ignore
)

// List is the resolved otel.ignore setting.
type List struct {
	Entries []string // as configured, for doctor
	Layer   string   // the config layer that set it, "" when unset
	// Invalid are entries that are neither a folder nor owner/repo: they match nothing, but
	// still make the list non-empty, so records no project can be told for are dropped.
	Invalid []string
	folders []string // absolute folder patterns, each as written and with symlinks resolved
	repos   []string // owner/repo patterns, lowercased
}

// FromSettings is the list res sets; home expands a leading "~/".
func FromSettings(res config.Resolved, home string) List {
	s, ok := res.Settings[Key]
	if !ok {
		return List{}
	}
	entries, _ := s.Value.([]string)
	if one, isStr := s.Value.(string); isStr {
		entries = []string{one}
	}
	l := List{Layer: s.Layer}
	for _, e := range entries {
		if e = strings.TrimSpace(e); e != "" {
			l.Entries = append(l.Entries, e)
			l.add(e, home)
		}
	}
	return l
}

// add files one entry as a folder or a repository pattern.
func (l *List) add(e, home string) {
	if e == "~" {
		e = home
	}
	if rest, ok := strings.CutPrefix(e, "~/"); ok {
		e = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(e) {
		repo := strings.ToLower(strings.Trim(e, "/"))
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.ContainsAny(owner[:1], ".~") ||
			badPattern(path.Match(repo, "")) {
			l.Invalid = append(l.Invalid, e)
			return
		}
		l.repos = append(l.repos, repo)
		return
	}
	e = filepath.Clean(e)
	if badPattern(filepath.Match(e, "")) {
		l.Invalid = append(l.Invalid, e)
		return
	}
	l.folders = append(l.folders, e)
	// macOS reports /tmp as /private/tmp: a folder written either way matches both.
	if real, ok := resolveFixedPrefix(e); ok && real != e {
		l.folders = append(l.folders, real)
	}
}

// badPattern is whether a Match call found its pattern malformed: such an entry matches nothing.
func badPattern(_ bool, err error) bool { return err != nil }

// hasGlob is whether s holds a character filepath.Match reads as a glob. A backslash is an
// escape there, except where it is the path separator.
func hasGlob(s string) bool {
	if filepath.Separator == '\\' {
		return strings.ContainsAny(s, "*?[")
	}
	return strings.ContainsAny(s, `*?[\`)
}

// resolveFixedPrefix is folder pattern e with symlinks resolved in its leading segments
// that hold no glob, the rest kept as written: EvalSymlinks fails on a literal glob.
func resolveFixedPrefix(e string) (string, bool) {
	fixed, rest := e, ""
	for hasGlob(fixed) {
		rest = filepath.Join(filepath.Base(fixed), rest)
		fixed = filepath.Dir(fixed)
	}
	real, err := filepath.EvalSymlinks(fixed)
	if err != nil {
		return "", false
	}
	return filepath.Join(real, rest), true
}

// Empty is a list that ignores nothing: the receiver then filters nothing.
func (l List) Empty() bool { return len(l.Entries) == 0 }

// Origin is a folder's git origin URL ("" when it has none), ok false when it could not be
// learned (git timed out).
type Origin func(dir string) (url string, ok bool)

// OriginTTL is how long OriginCache trusts a folder's origin.
const OriginTTL = 5 * time.Minute

// OriginCache asks git for a folder's origin at most once per OriginTTL: the receiver judges
// every export, and under load a git call that times out drops a project that is not listed.
// A lookup that timed out is not remembered.
type OriginCache struct {
	mu   sync.Mutex
	seen map[string]cachedOrigin
}

type cachedOrigin struct {
	url string
	at  time.Time
}

// Origin is dir's origin, from git or from a lookup less than OriginTTL old.
func (c *OriginCache) Origin(dir string) (string, bool) {
	now := time.Now()
	c.mu.Lock()
	o, ok := c.seen[dir]
	c.mu.Unlock()
	if ok && now.Sub(o.at) < OriginTTL {
		return o.url, true
	}
	g := gitinfo.NewUnbudgeted(io.Discard)
	u := g.Origin(dir)
	if g.TimedOut() {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]cachedOrigin{}
	}
	c.seen[dir] = cachedOrigin{u, now}
	return u, true
}

// Folder judges a session working in dir.
func (l List) Folder(dir string, origin Origin) Verdict {
	if !filepath.IsAbs(dir) {
		return Unknown
	}
	dirs := []string{filepath.Clean(dir)}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dirs[0] {
		dirs = append(dirs, real)
	}
	if slices.ContainsFunc(dirs, l.underFolder) {
		return Ignore
	}
	if len(l.repos) == 0 {
		return Keep
	}
	if _, err := os.Stat(dir); err != nil {
		return Unknown // a folder that is gone has no origin to tell its repository by
	}
	url, ok := origin(dir)
	if !ok {
		return Unknown
	}
	if _, owner, repo, parsed := gitinfo.RemoteOwnerRepo(url); parsed && l.namesRepo(owner+"/"+repo) {
		return Ignore
	}
	return Keep
}

// underFolder is whether d, or a folder above it, matches a folder entry.
func (l List) underFolder(d string) bool {
	for {
		for _, f := range l.folders {
			if ok, _ := filepath.Match(f, d); ok {
				return true
			}
		}
		up := filepath.Dir(d)
		if up == d {
			return false
		}
		d = up
	}
}

func (l List) namesRepo(slug string) bool {
	slug = strings.ToLower(slug)
	for _, p := range l.repos {
		if ok, _ := path.Match(p, slug); ok {
			return true
		}
	}
	return false
}
