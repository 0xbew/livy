package install

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/relay"
)

// --otel-to sink: Livy is its own OTLP receiver. The relay runs in sink mode as the same
// KeepAlive service on 127.0.0.1:14319, and stores each export, redacted, in the data folder's
// otel stream, which emit ships through the configured sink under source=otel/. Claude Code
// and Codex export OTLP/JSON to it with the relay key: Claude Code reads the key through its
// otelHeadersHelper, Codex from its [otel] table.

func receiverArgs() []string { return []string{"relay", "--sink", "--listen", RelayListen} }

// loadReceiver writes the key and loads the receiver, and waits for it to answer as itself.
func loadReceiver(p paths, o options, prev State) (Schedule, string, bool) {
	kind := relayHost()
	if kind == "" || o.noSchedule {
		return Schedule{}, "NOT configured: --otel-to sink needs a launch agent or systemd user service to keep Livy's receiver " +
			"running (not with --no-schedule); an export sent while it is down is lost", true
	}
	if o.dry {
		return prev.CodexRelay, "would load Livy's OTEL receiver (" + kind + ") on " + RelayListen + " and point Claude Code and Codex at it", false
	}
	table, _ := os.ReadFile(p.codex)
	if _, err := relayKey(hook.DataDir(), string(table)); err != nil {
		return Schedule{}, fmt.Sprintf("NOT configured: could not write the receiver's key: %v", err), true
	}
	loadedAt := clock.Now()
	sched, err := loadRelay(kind, p.home, p.bin, receiverArgs())
	if err == nil && !relayListening(loadedAt) {
		removeRelay(sched, false)
		err = errors.New("it loaded but is not listening on " + RelayListen + " (the port taken?)")
	}
	if err != nil {
		return Schedule{}, "NOT configured: Livy's OTEL receiver did not load (" + err.Error() + ")", true
	}
	return sched, "Livy's OTEL receiver (" + kind + ") on " + RelayListen + ", storing to " +
		relay.OtelStreamDir(hook.DataDir()) + " for the configured sink", false
}

// codexToReceiver points Codex's table at the receiver, OTLP/JSON, with the relay key.
func codexToReceiver(p paths, o options, prev State, st *State, text string) (string, bool) {
	key := relay.ReadKey(hook.DataDir())
	want := withCodexBlock(text, codexBlockAs("http://"+RelayListen, relay.KeyHeader, key, handleFor(o), codexSignals(o), "json"))
	if o.dry {
		if want != text {
			return "codex: would point the [otel] table in " + p.codex + " at Livy's OTEL receiver", false
		}
		return "codex: already exporting to Livy's OTEL receiver", false
	}
	if want == text {
		if prev.CodexRelaySince == "" {
			st.CodexRelaySince = clock.Now().UTC().Format(time.RFC3339)
		}
		return "codex: already exporting to Livy's OTEL receiver on " + RelayListen, false
	}
	if err := writeFileAtomic(p.codex, []byte(want), 0o600); err != nil {
		return fmt.Sprintf("codex: could not write %s: %v", p.codex, err), true
	}
	st.CodexRelaySince = clock.Now().UTC().Format(time.RFC3339)
	return "codex: exporting to Livy's OTEL receiver on " + RelayListen +
		"; restart Codex (the app and any codex sessions) so it reads the new table", false
}
