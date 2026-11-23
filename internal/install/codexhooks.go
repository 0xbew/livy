package install

import (
	"os"
	"path/filepath"
	"strings"
)

// Codex reads hooks from $CODEX_HOME/hooks.json in Claude Code's settings shape ({"hooks":
// {event: [groups]}}), so the same merge and strip keep everyone else's entries. Codex runs a
// hook only once it is trusted, and records that trust against the hook's hash: a new or
// changed entry is skipped until the engineer trusts it with /hooks in Codex.

// codexSessionEndTimeout is the longest Codex lets a SessionEnd hook run (seconds); it
// always runs SessionEnd hooks synchronously.
const codexSessionEndTimeout = 3

// CodexHooksName is the file in Codex's home that holds its user hooks.
const CodexHooksName = "hooks.json"

// codexHookArgs is `livy hook` for one event of Codex's.
func codexHookArgs(event string) string { return "hook --harness codex " + event }

// desiredCodexHooks are the Codex hook groups install adds, by event. Each line Codex's own
// shape can carry is written as Claude Code's is; tool.call is async, as it is written for
// every tool call and nothing waits on it. SessionEnd is always synchronous and cut at 3 s,
// so its upload is started in the background after session.ended is written.
func desiredCodexHooks(bin string) map[string][]*object {
	hook := func(event string, async bool) *object { return entry(bin, codexHookArgs(event), 5, async) }
	flush := func() *object { return entry(bin, "emit --extract --throttle "+throttleSeconds, 0, true) }
	end := entry(bin, codexHookArgs("session.ended")+"; "+shellQuote(bin)+" emit --extract </dev/null >/dev/null 2>&1 &",
		codexSessionEndTimeout, false)
	return map[string][]*object{
		"SessionStart":     {group("startup|resume|clear|fork", hook("session.started", false), entry(bin, "extract >/dev/null 2>&1", 0, true))},
		"UserPromptSubmit": {group("", hook("prompt.submitted", false), flush())},
		"PostToolUse":      {group("", hook("tool.call", true), flush())},
		"SubagentStart":    {group("", hook("agent.launched", false))},
		"Stop":             {group("", hook("turn.ended", false), flush())},
		"SessionEnd":       {group("", end)},
	}
}

// CodexRequired is, per Codex event, the Livy command capture depends on there: doctor's
// `codex hooks` row checks each is wired.
var CodexRequired = map[string]string{"SessionStart": codexHookArgs("session.started"),
	"UserPromptSubmit": codexHookArgs("prompt.submitted"), "PostToolUse": codexHookArgs("tool.call"),
	"SubagentStart": codexHookArgs("agent.launched"), "Stop": codexHookArgs("turn.ended"),
	"SessionEnd": codexHookArgs("session.ended")}

// hooksFile is a hooks file read for an edit: its object, its bytes as read (so the write
// keeps their layout), and whether it was missing.
type hooksFile struct {
	path  string
	doc   *object
	raw   []byte
	fresh bool
}

func readHooksFile(path string) (hooksFile, error) {
	doc, fresh, err := readSettings(path)
	raw, _ := os.ReadFile(path)
	return hooksFile{path: path, doc: doc, raw: raw, fresh: fresh}, err
}

// edit applies change to the object and says whether it changed anything.
func (h hooksFile) edit(change func(*object)) bool {
	before := encode(h.doc)
	change(h.doc)
	return string(before) != string(encode(h.doc))
}

func (h hooksFile) write() error { return writeFileAtomic(h.path, encodeLike(h.raw, h.doc), 0o600) }

// codexHooks is step 4a: Livy's entries in Codex's hooks.json, when Codex is on this machine.
func (r *installRun) codexHooks() {
	dir := filepath.Dir(r.p.codex)
	if _, err := os.Stat(dir); err != nil {
		r.say("codex hooks: Codex not on this machine (%s), skipped", dir)
		return
	}
	h, err := readHooksFile(filepath.Join(dir, CodexHooksName))
	if err != nil {
		r.fail("codex hooks: %s is not usable (%v); left alone, Codex sessions are not recorded", h.path, err)
		return
	}
	var refused []string
	changed := h.edit(func(doc *object) { refused = mergeHooks(doc, desiredCodexHooks(r.p.bin)) })
	if len(refused) > 0 {
		r.fail("codex hooks: hooks for %s hold something that is not a list; left alone", strings.Join(refused, ", "))
	}
	switch {
	case !changed && !h.fresh:
		r.say("codex hooks: already in place in %s", h.path)
	case r.o.dry:
		r.say("codex hooks: would update %s", h.path)
		return
	default:
		if err := h.write(); err != nil {
			r.fail("codex hooks: could not write %s: %v", h.path, err)
			return
		}
		r.say("codex hooks: written to %s; Codex runs them once trusted: open Codex, run /hooks and trust Livy's", h.path)
	}
	r.st.CodexHooks = h.path
	r.st.CreatedCodexHooks = h.fresh || (r.prev.CreatedCodexHooks && r.prev.CodexHooks == h.path)
}

// codexHooks takes Livy's entries out of Codex's hooks.json, and removes the file when
// install created it and nothing else is left.
func (u *uninstallRun) codexHooks() {
	path := u.st.CodexHooks
	if path == "" {
		path = filepath.Join(filepath.Dir(u.p.codex), CodexHooksName)
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	h, err := readHooksFile(path)
	if err != nil {
		u.fail("%s is not usable (%v); Livy's Codex hooks in it were NOT removed", path, err)
		return
	}
	if !h.edit(stripHooks) {
		return
	}
	switch {
	case u.o.dry:
		u.say("would remove Livy's entries from %s", path)
	case u.st.CreatedCodexHooks && len(h.doc.keys) == 0:
		if err := os.Remove(path); err != nil {
			u.fail("could not remove %s: %v", path, err)
		} else {
			u.say("removed %s (install created it)", path)
		}
	default:
		if err := h.write(); err != nil {
			u.fail("could not write %s: %v", path, err)
		} else {
			u.say("removed Livy's entries from %s", path)
		}
	}
}
