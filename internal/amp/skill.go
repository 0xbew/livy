package amp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/0xbew/livy/internal/buildinfo"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/skill"
)

// skill.loaded is written for each skill call Amp completed, with the same fields and rules as
// Claude Code's (internal/hook/skill.go): the skill's name, its scope, and the sha256 of SKILL.md
// read from disk right after the load. Never written: the skill's text, the call's arguments, a
// path, or a plugin's name unless it is public. Unlike Claude Code, Amp names the folder it
// loaded from, so the folder is not looked up: it is hashed as Amp reports it.
const (
	skillLoadedEvent  = "skill.loaded"
	triggerAgent      = "agent"
	hashDiskAfterLoad = "disk-after-load"
	// officialOwner is the owner folder Amp keeps its own skills under, in its global-skills cache:
	// <cache>/amp/global-skills/<host>/official/<name>@<version>.
	officialOwner = "official"
)

// skillMarkers are the folders that hold a project's skills folder: <folder>/<marker>/skills/<name>.
var skillMarkers = map[string]bool{".agents": true, ".claude": true}

// skillLoaded writes skill.loaded for a completed skill call, and a ledger.error for a load whose
// facts are missing or that it could not hash. A call that failed leaves nothing.
func (w *writer) skillLoaded(e toolResult) error {
	if e.Status != "done" {
		return nil
	}
	if e.Skill == nil {
		w.rec.Error("amp: skill.loaded not recorded: livy.ts forwarded no skill (a plugin older than template 4; re-run livy install)", true)
		return nil
	}
	if e.Skill.Name == "" {
		w.rec.Error("amp: skill.loaded not recorded: the skill call has no input name (Amp's payload changed?)", true)
		return nil
	}
	n, ok := skill.ParseName(e.Skill.Name)
	if !ok {
		w.rec.Error("amp: skill.loaded not recorded: the skill call's skill is not a plain name", true)
		return nil
	}
	rec, err := w.line(skillLoadedEvent, e.Thread.ID)
	if err != nil {
		return err
	}
	rec["trigger"] = triggerAgent
	if e.ToolUseID != "" {
		rec["tool_use_id"] = e.ToolUseID
	}
	if b, ok := buildinfo.Read(); ok {
		rec["recorder_build"] = b.Rev
	}
	why := fillSkill(rec, n, e.Skill.Dir, places{top: w.rec.Toplevel(), home: hook.Home(), cache: ampCacheDir()})
	if err := w.append(rec); err != nil {
		return err
	}
	if why != "" {
		w.rec.Error("amp: skill.loaded "+n.Base+": "+why, true)
	}
	return nil
}

// places are the folders a skill's scope is read from.
type places struct {
	top   string // the thread's repository root, or ""
	home  string
	cache string // Amp's cache folder
}

// fillSkill adds the skill's name, scope and hashes to rec, and returns why one is missing ("" when
// none is). The reason never names a path.
func fillSkill(rec map[string]any, n skill.Name, dir string, p places) string {
	if !filepath.IsAbs(dir) {
		var f skill.Found
		if n.Prefix != "" {
			f.Scope = skill.ScopePlugin // scopeOf's rule for a prefixed name needs no folder
		}
		rec["skill"] = skill.LineName(n, f)
		return "Amp named no folder for the skill; recorded without a scope or hash"
	}
	scope, folder := scopeOf(n, dir, p)
	rec["skill"] = skill.LineName(n, skill.Found{Scope: scope})
	var why string
	if scope == "" {
		why = "the skill's folder is in no place Livy knows; recorded without a scope"
	} else {
		rec["scope"] = string(scope)
	}
	if folder != "" {
		rec["scope_dir_hash"] = skill.HashString(folder)
	}
	h, err := skill.HashDir(dir)
	if err != nil {
		return joinWhy(why, "SKILL.md could not be read; recorded without a hash")
	}
	rec["sha256"] = h.SkillMD
	rec["hash_method"] = hashDiskAfterLoad
	switch {
	case errors.Is(h.TreeErr, skill.ErrTreeTooLarge):
		return joinWhy(why, "the skill folder is too large to hash in a hook; tree_sha256 left out")
	case h.TreeErr != nil:
		return joinWhy(why, "the skill folder could not be read in full; tree_sha256 left out")
	}
	rec["tree_sha256"] = h.Tree
	return why
}

func joinWhy(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// scopeOf is where an Amp skill came from, and for a project skill below the repository root, the
// repo-relative folder it belongs to. Scope "" is a folder in no place Livy knows. A `plugin:name`
// skill is a plugin's, wherever its folder is. Amp's global-skills cache is checked before home,
// which holds it; a skill there that is not Amp's own is in no place Livy knows.
func scopeOf(n skill.Name, dir string, p places) (skill.Scope, string) {
	if n.Prefix != "" {
		return skill.ScopePlugin, ""
	}
	dir = resolved(dir)
	if rel, ok := under(dir, filepath.Join(p.cache, "global-skills")); ok {
		parts := strings.Split(rel, "/")
		if len(parts) > 2 && parts[1] == officialOwner {
			return skill.ScopeBuiltin, ""
		}
		return "", ""
	}
	if _, ok := under(dir, p.top); ok {
		return skill.ScopeProject, projectFolder(dir, resolved(p.top))
	}
	if _, ok := under(dir, p.home); ok {
		return skill.ScopeUser, ""
	}
	return "", ""
}

// projectFolder is the repo-relative folder of <top>/<folder>/<marker>/skills/<name>, "" for the
// root's own skills or a layout Amp's project skills do not have.
func projectFolder(dir, top string) string {
	skills := filepath.Dir(dir)
	marker := filepath.Dir(skills)
	if filepath.Base(skills) != "skills" || !skillMarkers[filepath.Base(marker)] {
		return ""
	}
	rel, ok := under(filepath.Dir(marker), top)
	if !ok || rel == "." {
		return ""
	}
	return rel
}

// under is dir relative to root (slash-separated) when dir is root or inside it.
func under(dir, root string) (string, bool) {
	if root == "" {
		return "", false
	}
	rel, err := filepath.Rel(resolved(root), dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// resolved is p with its symlinks followed (macOS's /tmp is /private/tmp), or p when it cannot be.
func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// ampCacheDir is the cache folder Amp keeps its global skills in: $XDG_CACHE_HOME/amp, else
// ~/.cache/amp, on every platform (Amp does not use macOS's ~/Library/Caches).
func ampCacheDir() string {
	if d := os.Getenv("XDG_CACHE_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, "amp")
	}
	return filepath.Join(hook.Home(), ".cache", "amp")
}
