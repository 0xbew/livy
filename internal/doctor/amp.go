package doctor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/amp"
	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/install"
)

// checkAmp adds the Amp rows, when install wrote the Amp plugin.
func checkAmp(ctx context.Context, r *Report) {
	st, _ := install.Recorded()
	if st.AmpPlugin == "" {
		return
	}
	dir := hook.DataDir()
	written, ok := checkAmpFile(r, st.AmpPlugin, st.Binary, dir)
	if !ok {
		return
	}
	checkAmpLoaded(ctx, r, dir, st.AmpPlugin)
	if dead := amp.DeadMarkers(filepath.Join(dir, amp.StateDir), 0); len(dead) > 0 {
		r.add(Warn, "amp unclean exits", "%d Amp plugin process(es) ended without a clean shutdown; events they held may be lost (recorded at the plugin's next load)", len(dead))
	} else {
		r.add(OK, "amp unclean exits", "none waiting to be recorded")
	}
	r.add(OK, "amp instructions", "not available on this harness: Amp does not report which AGENTS.md files a thread loaded")
	checkAmpSessions(ctx, r, written)
}

// checkAmpFile is the `amp plugin` row: the file is there, ours, names this binary and is the
// current template. It returns when the file was last written, and false when it is unusable.
func checkAmpFile(r *Report, path, bin, dataDir string) (time.Time, bool) {
	have, err := os.ReadFile(path)
	if err != nil {
		r.add(Fail, "amp plugin", "%s is missing (%v): Amp is not recorded; re-run livy install", path, err)
		return time.Time{}, false
	}
	if !bytes.HasPrefix(have, []byte(install.AmpMarker+"\n")) {
		r.add(Fail, "amp plugin", "%s is not Livy's (no marker): Amp is not recorded", path)
		return time.Time{}, false
	}
	var written time.Time
	if fi, err := os.Stat(path); err == nil {
		written = fi.ModTime()
	}
	if !bytes.Equal(have, install.AmpPlugin(bin, dataDir)) {
		r.add(Warn, "amp plugin", "%s is out of date or names another livy binary; re-run livy install", path)
		return written, true
	}
	r.add(OK, "amp plugin", "%s, template %s", path, install.AmpTemplateVersion)
	return written, true
}

// checkAmpLoaded is the `amp plugin loaded` row: Amp lists the plugin file as active. Amp lists
// a plugin by its path (`✓ .amp/plugins/dump.ts active` for a project plugin), so the row looks
// for a line naming livy.ts as active. A load time is not shown: running `amp plugins` loads the
// plugin too, so only a recorded turn says Amp was used.
func checkAmpLoaded(ctx context.Context, r *Report, dataDir, pluginPath string) {
	last := "no Amp turn recorded yet"
	if st, _ := amp.ReadState(dataDir); st.LastTurn != "" {
		last = "last turn recorded " + st.LastTurn
	}
	out, err := output(ctx, "amp", "plugins", "list")
	if err != nil {
		r.add(Warn, "amp plugin loaded", "`amp plugins list` did not run (%v); %s", err, last)
		return
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if listsActive(line, pluginPath, hook.Home()) {
			r.add(OK, "amp plugin loaded", "Amp lists it as active; %s", last)
			return
		}
	}
	r.add(Warn, "amp plugin loaded", "`amp plugins list` does not list livy.ts as active: a syntax error, or Amp not restarted; %s", last)
}

// checkAmpSessions is the `amp sessions` row: an Amp started before the plugin was written
// has not loaded it.
func checkAmpSessions(ctx context.Context, r *Report, written time.Time) {
	if written.IsZero() {
		return
	}
	out, err := output(ctx, "ps", "-A", "-o", "pid=,etime=,comm=")
	if err != nil {
		r.add(Warn, "amp sessions", "could not list processes, so an Amp started before the plugin would not show here")
		return
	}
	if stale := staleProcesses(string(out), "amp", clock.Now(), written); len(stale) > 0 {
		r.add(Warn, "amp sessions", "%d Amp process(es) started before the plugin was written (pid %s) and are not recorded; restart them",
			len(stale), strings.Join(stale, ", "))
		return
	}
	r.add(OK, "amp sessions", "no running Amp predates the plugin")
}

// listsActive says an `amp plugins list` line shows the plugin at pluginPath as active: a status
// mark, the plugin's path (Amp may write the home folder as ~), then the word `active` alone, so
// `inactive` and a line about another livy.ts do not count. The path may hold spaces.
func listsActive(line, pluginPath, home string) bool {
	rest, ok := strings.CutSuffix(strings.TrimSpace(line), " active")
	if !ok || pluginPath == "" {
		return false
	}
	_, path, ok := strings.Cut(rest, " ") // after the status mark
	if !ok {
		return false
	}
	path = strings.TrimSpace(path)
	if tail, ok := strings.CutPrefix(path, "~/"); ok {
		path = filepath.Join(home, tail)
	}
	return path == pluginPath
}
