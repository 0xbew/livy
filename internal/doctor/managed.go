package doctor

import (
	"fmt"
	"maps"
	"strings"

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
// nothing, since Claude Code does not take it.
func checkManagedOtel(r *Report, m ccpolicy.Settings, st install.State) bool {
	broken := false
	for _, o := range m.Overrides() {
		status := OK
		if o.Breaks {
			status, broken = Fail, true
		}
		r.add(status, "otel managed settings", "%s set in %s: %s", o.Key, strings.Join(m.Files, ", "), o.Why)
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
	r.add(Fail, "otel", "Claude Code's managed settings override Livy's OTEL route (rows above), so Claude Code's OTEL is not "+
		"reaching Livy on this machine; %s. A managed key wins over the user settings; `livy install --otel-around-managed` "+
		"writes the per-signal keys the managed settings leave unset", counts)
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
