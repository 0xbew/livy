package doctor

import (
	"fmt"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/ccpolicy"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/install"
	"github.com/0xbew/livy/internal/relay"
)

// effectiveEnv is the env Claude Code applies: the user settings' env with the managed
// settings' over it, key by key, less the managed keys the managed settings rows judge.
func effectiveEnv(user map[string]any, m ccpolicy.Settings) map[string]any {
	env := maps.Clone(user)
	if env == nil {
		env = map[string]any{}
	}
	for k, v := range m.Env {
		env[k] = v
	}
	for _, o := range m.Overrides() {
		delete(env, o.Key)
	}
	return env
}

// readManaged reads Claude Code's managed settings, adding a row when they cannot be read.
func readManaged(r *Report, dir string) ccpolicy.Settings {
	m, err := ccpolicy.Read(dir)
	if err != nil {
		r.add(Warn, "claude managed settings", "could not be read (%v): whether they override Livy's OTEL route or hooks is unknown", err)
	}
	return m
}

// checkManagedOtel adds a row for each key of Claude Code's managed settings that overrides
// Livy's OTEL route, and is whether one takes it away: then a probe of Livy's own route proves
// nothing, since Claude Code does not take it. Such a key fails only while Livy's own hooks
// cannot time Claude Code instead (hookTiming): the person cannot change company settings, and
// a row that always fails trains them to ignore doctor.
func checkManagedOtel(r *Report, m ccpolicy.Settings, st install.State, settingsPath string) bool {
	broken := routesElsewhere(m)
	timed, timing := false, ""
	if broken {
		timed, timing = hookTiming(m, settingsPath, hook.DataDir())
	}
	for _, o := range m.Overrides() {
		switch {
		case !o.Breaks:
			r.add(OK, "otel managed settings", "%s set in %s: %s", o.Key, strings.Join(m.Files, ", "), o.Why)
		case timed:
			r.add(OK, "otel managed settings", "%s set in %s: %s. Company settings route Claude Code's OTEL elsewhere; "+
				"Livy times Claude Code from its own hooks instead", o.Key, strings.Join(m.Files, ", "), o.Why)
		default:
			r.add(Fail, "otel managed settings", "%s set in %s: %s", o.Key, strings.Join(m.Files, ", "), o.Why)
		}
	}
	if !broken {
		return false
	}
	counts := receiverCounts(st)
	if _, blocked := m.PerSignal(); len(st.RelayKeyEnv) > 0 && blocked != "" {
		r.add(Fail, "otel", "livy install --otel-around-managed wrote per-signal keys (%s), but no route around the managed settings "+
			"is left: %s; %s", strings.Join(st.RelayKeyEnv, ", "), blocked, counts)
		return true
	}
	if len(st.RelayKeyEnv) > 0 {
		r.add(Warn, "otel", "Claude Code's managed settings override Livy's OTEL route (rows above); livy install --otel-around-managed "+
			"sends traces and logs to Livy by the per-signal keys %s, which Claude Code versions that guard a managed headers "+
			"helper drop as well; %s", strings.Join(st.RelayKeyEnv, ", "), counts)
		return true
	}
	if timed {
		r.add(OK, "otel", "company settings route Claude Code's OTEL elsewhere (rows above), so Livy times Claude Code from its "+
			"own hooks instead: %s; %s", timing, counts)
		return true
	}
	r.add(Fail, "otel", "Claude Code's managed settings override Livy's OTEL route (rows above), so Claude Code's OTEL is not "+
		"reaching Livy on this machine, and Livy's own hooks cannot time it instead: %s; %s. A managed key wins over the user "+
		"settings; `livy install --otel-around-managed` writes the per-signal keys the managed settings leave unset", timing, counts)
	return true
}

// receiverCounts is what Livy's receiver stored and refused, for an export that cannot be probed.
func receiverCounts(st install.State) string {
	if st.OtelDestination != "sink" {
		return "Livy's receiver is not in use"
	}
	s := relay.ReadState(hook.DataDir())
	return fmt.Sprintf("Livy's receiver has stored %d export(s) (last %s) and refused %d for a missing or wrong relay key (last %s) since %s",
		s.Delivered, firstNonEmpty(s.LastDeliveredAt, "never"), s.Refused, firstNonEmpty(s.LastRefusedAt, "never"),
		firstNonEmpty(s.CountingSince, s.StartedAt, "it started"))
}

// checkManagedHooks fails when Claude Code will not run Livy's user hooks: the managed
// settings allow only their own, or a layer turns every hook off.
func checkManagedHooks(r *Report, m ccpolicy.Settings, userNoHooks bool) {
	for _, o := range m.HookBlocks() {
		r.add(Fail, "hooks", "%s set in Claude Code's managed settings (%s): %s", o.Key, strings.Join(m.Files, ", "), o.Why)
	}
	if userNoHooks && m.NoHooks == nil {
		r.add(Fail, "hooks", "disableAllHooks is set in Claude Code's user settings: no hook runs, so the ledger records no Claude Code session")
	}
}

// refusalCauses names what may have sent the relay requests without its key. Claude Code is
// one only while it may still export to Livy: managed settings that route its OTEL elsewhere
// and last changed after the last refusal rule it out, as the refusal times show.
func refusalCauses(lastRefused time.Time, m ccpolicy.Settings) string {
	const others = "a Codex process started before the key (restart it), or another local process"
	if changed := managedChanged(m); routesElsewhere(m) && lastRefused.Before(changed) {
		return fmt.Sprintf("%s; not Claude Code as it runs now: none since the managed settings that route its OTEL elsewhere "+
			"last changed (%s)", others, changed.UTC().Format(time.RFC3339))
	}
	return "a Codex process started before the key (restart it), Claude Code exporting with headers that are not Livy's " +
		"(see the otel managed settings rows), or another local process"
}

// routesElsewhere says a managed key takes Claude Code's OTEL route away from Livy.
func routesElsewhere(m ccpolicy.Settings) bool {
	for _, o := range m.Overrides() {
		if o.Breaks {
			return true
		}
	}
	return false
}

// managedChanged is when the managed settings files last changed; zero when none can be read.
func managedChanged(m ccpolicy.Settings) time.Time {
	var newest time.Time
	for _, f := range m.Files {
		if fi, err := os.Stat(f); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest
}
