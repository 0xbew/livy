// Package skill finds the SKILL.md Claude Code loaded for a skill name and hashes it, so a
// ledger line can say which version of a skill a session ran without carrying its text,
// its arguments or where it lives.
package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/0xbew/livy/internal/plainname"
)

// Scope is where a skill came from, as Claude Code ranks the places it looks.
type Scope string

// The scopes a skill.loaded line may carry.
const (
	ScopeManaged Scope = "managed"
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
	ScopePlugin  Scope = "plugin"
	// ScopeBuiltin is a skill the harness ships itself (Amp's official skills).
	ScopeBuiltin Scope = "builtin"
)

// skillFile is the file Claude Code loads from a skill's folder.
const skillFile = "SKILL.md"

// skillsDir is the folder, under a project or managed root, that holds skill folders.
var skillsDir = filepath.Join(".claude", "skills")

// publicPlugins are plugin names public enough to keep in clear on a ledger line; any other
// plugin name goes out as its sha256.
var publicPlugins = map[string]bool{"anthropic-skills": true}

// Name is a skill name as Claude Code passes it: `deploy`, `apps/web:deploy` for a skill
// scoped to a folder, or `plugin:deploy`.
type Name struct {
	Prefix string // "" for a plain name; a repo-relative folder or a plugin name otherwise
	Base   string
}

// ParseName splits s into its parts. ok is false when the skill part is not a plain name or
// the prefix could leave the repository (absolute, or a `..` segment).
func ParseName(s string) (Name, bool) {
	prefix, base, found := strings.Cut(s, ":")
	if !found {
		prefix, base = "", s
	}
	if !plainname.Valid(base) || strings.ContainsAny(prefix, " \t\n\r\\") {
		return Name{}, false
	}
	if found && (prefix == "" || path.IsAbs(prefix) || path.Clean(prefix) != prefix ||
		prefix == ".." || strings.HasPrefix(prefix, "../")) {
		return Name{}, false
	}
	return Name{Prefix: prefix, Base: base}, true
}

// Places are where Claude Code looks for a skill on this machine.
type Places struct {
	Managed  string // the managed settings folder ("" when the platform has none)
	User     string // the Claude Code config folder (~/.claude or $CLAUDE_CONFIG_DIR)
	Cwd      string // the session's working directory
	Toplevel string // its repository root, or ""
	// Nested is the repo-relative folders below Toplevel holding .claude/skills/<base>/SKILL.md.
	// It is asked only when no other place has the skill.
	Nested func(base string) []string
}

// Found is the skill folder a name resolved to.
type Found struct {
	Scope Scope
	Dir   string // the skill's folder; "" when its files are not on disk where Livy can read them
	// Folder is the repo-relative folder a folder-scoped skill belongs to ("" otherwise).
	Folder string
	// Ambiguous is true when more than one nested folder holds a skill of this plain name.
	Ambiguous bool
}

// Resolve finds n among places, highest-ranked first: managed, then user, then the project
// (the working directory up to the repository root), then a folder-scoped skill below it.
// ok is false when no place holds it.
func Resolve(n Name, p Places) (Found, bool) {
	if n.Prefix != "" {
		return resolvePrefixed(n, p)
	}
	if p.Managed != "" {
		if d := skillDir(filepath.Join(p.Managed, skillsDir), n.Base); d != "" {
			return Found{Scope: ScopeManaged, Dir: d}, true
		}
	}
	if p.User != "" {
		if d := skillDir(filepath.Join(p.User, "skills"), n.Base); d != "" {
			return Found{Scope: ScopeUser, Dir: d}, true
		}
	}
	if f, ok := resolveProject(n.Base, p); ok {
		return f, true
	}
	if p.Toplevel == "" || p.Nested == nil {
		return Found{}, false
	}
	var live []Found
	for _, folder := range p.Nested(n.Base) {
		// git lists a tracked SKILL.md deleted from the working tree; only one on disk counts.
		if f := projectFolder(p.Toplevel, folder, n.Base); f.Dir != "" {
			live = append(live, f)
		}
	}
	switch len(live) {
	case 0:
		return Found{}, false
	case 1:
		return live[0], true
	default:
		return Found{Scope: ScopeProject, Ambiguous: true}, true
	}
}

// resolvePrefixed is a `folder:name` skill when that folder holds it, else a plugin's.
func resolvePrefixed(n Name, p Places) (Found, bool) {
	if p.Toplevel != "" {
		if f := projectFolder(p.Toplevel, n.Prefix, n.Base); f.Dir != "" {
			return f, true
		}
	}
	if strings.Contains(n.Prefix, "/") {
		return Found{}, false
	}
	return Found{Scope: ScopePlugin}, true
}

// resolveProject walks from the working directory up to the repository root, nearest first.
func resolveProject(base string, p Places) (Found, bool) {
	if p.Cwd == "" {
		return Found{}, false
	}
	for dir := p.Cwd; ; dir = filepath.Dir(dir) {
		if d := skillDir(filepath.Join(dir, skillsDir), base); d != "" {
			f := Found{Scope: ScopeProject, Dir: d}
			if rel, err := filepath.Rel(p.Toplevel, dir); p.Toplevel != "" && err == nil && rel != "." {
				f.Folder = filepath.ToSlash(rel)
			}
			return f, true
		}
		if p.Toplevel == "" || dir == p.Toplevel || dir == filepath.Dir(dir) {
			return Found{}, false
		}
	}
}

// projectFolder is the skill base under the repo-relative folder, Dir "" when absent.
func projectFolder(top, folder, base string) Found {
	d := skillDir(filepath.Join(top, filepath.FromSlash(folder), skillsDir), base)
	return Found{Scope: ScopeProject, Dir: d, Folder: folder}
}

// skillDir is root/base when it holds a SKILL.md, else "".
func skillDir(root, base string) string {
	d := filepath.Join(root, base)
	if st, err := os.Stat(filepath.Join(d, skillFile)); err == nil && st.Mode().IsRegular() {
		return d
	}
	return ""
}

// LineName is the skill as a ledger line carries it: the plain name, a plugin's skill
// prefixed by the plugin's name if it is public and by its sha256 otherwise, and a
// folder-scoped skill without its folder (which goes out only as FolderHash).
func LineName(n Name, f Found) string {
	if f.Scope != ScopePlugin {
		return n.Base
	}
	if publicPlugins[n.Prefix] {
		return n.Prefix + ":" + n.Base
	}
	return HashString(n.Prefix) + ":" + n.Base
}

// HashString is the hex sha256 of s.
func HashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
