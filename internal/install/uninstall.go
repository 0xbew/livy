package install

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/0xbew/livy/internal/hook"
)

// Uninstall is `livy uninstall [--purge] [--repo R]`: removes what install added (by its
// state file, then by marker), and the captured data only with --purge.
func Uninstall(args []string, stdout, stderr io.Writer) int {
	o, err := parse("uninstall", translatePyArgs(args), stderr)
	if err != nil {
		return 2
	}
	p := where()
	st := loadState(p.statePath)
	if st.Settings != "" {
		p.settings = st.Settings
	}
	if st.CodexConfig != "" {
		p.codex = st.CodexConfig
	}
	u := &uninstallRun{o: o, p: p, st: st, stdout: stdout}
	u.claudeSettings()
	u.commandLink()
	for _, n := range removeRelay(st.CodexRelay, o.dry) {
		u.note(n)
	}
	u.codexConfig()
	u.codexHooks()
	u.ampPlugin()
	for _, r := range st.HookRedirects {
		msg, bad := removeRedirect(r, o.dry)
		u.failed = u.failed || bad
		u.say("git: %s", msg)
	}
	u.gitHooks()
	if u.st.Schedule.Kind == "" {
		u.st.Schedule = guessSchedule(p.home)
	}
	for _, n := range removeSchedule(u.st.Schedule, o.dry) {
		u.note(n)
	}
	u.binaryAndRecord()
	u.capturedData()
	if u.failed {
		return 1
	}
	return 0
}

// uninstallRun is one Uninstall: what install recorded (st), and whether any step failed.
type uninstallRun struct {
	o      options
	p      paths
	st     State
	stdout io.Writer
	failed bool
}

func (u *uninstallRun) say(format string, a ...any) { fmt.Fprintf(u.stdout, format+"\n", a...) }

func (u *uninstallRun) fail(format string, a ...any) {
	u.failed = true
	u.say(format, a...)
}

// note reports one removal step's outcome.
func (u *uninstallRun) note(n note) {
	u.failed = u.failed || n.failed
	u.say("%s", n.text)
}

// claudeSettings takes Livy's hooks, env and headers helper out of Claude Code's settings,
// and removes the file when install created it and nothing else is left.
func (u *uninstallRun) claudeSettings() {
	p, st := u.p, u.st
	settings, _, err := readSettings(p.settings)
	rawSettings, _ := os.ReadFile(p.settings)
	if err != nil {
		u.fail("%s is not usable (%v); Livy's hooks in it were NOT removed", p.settings, err)
		return
	}
	before := encode(settings)
	stripHooks(settings)
	unsetOwned(settings, "env", st.Env)
	dropHelper(settings, st.HeadersHelper)
	if string(before) == string(encode(settings)) {
		return
	}
	switch {
	case u.o.dry:
		u.say("would remove Livy's entries from %s", p.settings)
	case st.CreatedSettings && len(settings.keys) == 0:
		if err := os.Remove(p.settings); err != nil {
			u.fail("could not remove %s: %v", p.settings, err)
		} else {
			u.say("removed %s (install created it)", p.settings)
		}
	default:
		if err := writeFileAtomic(p.settings, encodeLike(rawSettings, settings), 0o600); err != nil {
			u.fail("could not write %s: %v", p.settings, err)
		} else {
			u.say("removed Livy's entries from %s", p.settings)
		}
	}
}

// commandLink removes the `livy` command link install made.
func (u *uninstallRun) commandLink() {
	cl := u.st.CommandLink
	if cl == "" {
		cl = filepath.Join(linkDir(u.p.home), linkName)
	}
	if n := removeLink(cl, u.p.bin, u.o.dry); n.text != "" {
		u.failed = u.failed || n.failed
		u.say("command: %s", n.text)
	}
}

// codexConfig removes Livy's [otel] table from Codex's config.
func (u *uninstallRun) codexConfig() {
	path := u.p.codex
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), codexMarker) {
		return
	}
	if u.o.dry {
		u.say("would remove the [otel] table from %s", path)
	} else if err := writeFileAtomic(path, []byte(withoutCodexBlock(string(raw))), 0o600); err != nil {
		u.fail("could not write %s: %v", path, err)
	} else {
		u.say("removed the [otel] table from %s", path)
	}
}

// gitHooks removes the git hooks install recorded, and those of each --repo named.
func (u *uninstallRun) gitHooks() {
	hooks := append([]string(nil), u.st.GitHooks...)
	for _, r := range u.o.repos {
		if path, _, err := hookPath(r); err == nil {
			hooks = appendNew(appendNew(hooks, path), filepath.Join(filepath.Dir(path), trailerHook))
			for _, name := range commitHooks {
				hooks = appendNew(hooks, filepath.Join(filepath.Dir(path), name))
			}
		}
	}
	for _, h := range hooks {
		if n := removeGitHook(h, u.o.dry); n.text != "" {
			u.note(n)
		}
	}
}

// binaryAndRecord removes the binary and the install record, unless a step above failed.
func (u *uninstallRun) binaryAndRecord() {
	for _, f := range []string{u.p.bin, u.p.statePath} {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		if u.failed {
			// Something above was not removed: keep the record of what install owns, and the
			// binary its remaining hooks call, so a later uninstall can finish the job.
			u.say("kept %s: uninstall did not finish; run it again once the problem above is fixed", f)
			continue
		}
		if u.o.dry {
			u.say("would remove %s", f)
		} else if err := os.Remove(f); err != nil {
			u.fail("could not remove %s: %v", f, err)
		} else {
			u.say("removed %s", f)
		}
	}
	if !u.o.dry && !u.failed {
		_ = os.Remove(filepath.Dir(u.p.bin)) // only when empty
	}
}

// capturedData deletes Livy's captured data with --purge, and says it was kept otherwise.
func (u *uninstallRun) capturedData() {
	data := hook.DataDir()
	switch {
	case u.o.purge && u.o.dry:
		u.say("would delete Livy's files in %s and %s", data, u.p.userDir)
	case u.o.purge:
		left, err := purge(data, u.p.userDir, hook.LegacyDir(), u.failed)
		switch {
		case err != nil:
			u.fail("could not delete the captured data in %s: %v", data, err)
		case len(left) > 0:
			u.say("deleted Livy's files; kept: %s", strings.Join(left, ", "))
		default:
			u.say("deleted the captured data in %s", data)
		}
	default:
		u.say("kept the captured data in %s (--purge deletes it)", data)
	}
}
