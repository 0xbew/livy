// Package amp is `livy amp <event> [key=value ...]`: one Amp plugin event, handed over by the
// livy.ts plugin `livy install` writes, becomes ledger lines. The plugin's handlers return at
// once and run this as a child process with the workspace root as its folder, so the scope,
// the branch and the ledger write are the hook's own (hook.Recorder): out of scope leaves no
// trace, a line that cannot be written becomes a ledger.error, and every path exits 0.
//
// Amp reports no loaded instruction files, no model switch and no session end, so no such
// line is ever written for it; every session.started says so in not_observable. A skill is
// seen when Amp's skill tool loads it, and written as skill.loaded.
package amp

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/pytext"
)

// harness names Amp on every line.
const harness = "amp"

// maxHeld bounds how old an event's own time may be and still be used.
const maxHeld = 7 * 24 * time.Hour

// options are the key=value arguments after the event name: counts the plugin carries
// from failures it could not report itself, and whether to start an upload afterwards
// (flush=throttled, or flush=now for the last upload before an orb pauses).
type options struct {
	flush, now                                    bool
	dropped, handlerErrors, spillFailed, noFolder int
}

func parseOptions(args []string) options {
	var o options
	for _, a := range args {
		k, v, _ := strings.Cut(a, "=")
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			n = 0
		}
		switch k {
		case "flush":
			o.flush = v == "throttled" || v == "now"
			o.now = v == "now"
		case "dropped":
			o.dropped = n
		case "handler_errors":
			o.handlerErrors = n
		case "spill_failed":
			o.spillFailed = n
		case "no_folder":
			o.noFolder = n
		}
	}
	return o
}

// Main handles one `livy amp` invocation and returns the exit code, always 0. The plugin
// reads any other code as a crash and records that itself.
func Main(args []string, stdin *os.File, stderr io.Writer) (code int) {
	defer func() {
		if p := recover(); p != nil {
			code = 0
			defer func() { _ = recover() }()
			hook.WriteError(fmt.Sprintf("amp: unhandled panic: %T", p), stderr)
		}
	}()
	if len(args) == 0 {
		hook.WriteError("livy amp invoked with no event name", stderr)
		return 0
	}
	name, o := args[0], parseOptions(args[1:])
	// Before the scope check: a count of lost events names no repository, and the plugin has
	// already handed it over.
	reportCounts(o, stderr)
	defer func() {
		if o.flush {
			startUpload(o.now, stderr)
		}
	}()
	switch name {
	case "flush":
		o.flush = true
		return 0
	case "loaded":
		loaded(stdin, stderr)
		return 0
	}
	rec := hook.NewRecorder(stderr)
	if !rec.InScope() {
		if _, timedOut := rec.GitTimeout(); timedOut {
			rec.GitTimedOutError("amp: ", "event "+name+" not recorded")
		}
		return 0
	}
	obj, problem := rec.Payload(stdin)
	if problem == "" && obj == nil {
		problem = "no event on stdin"
	}
	if problem != "" {
		rec.Error(fmt.Sprintf("amp: %s (event %s not recorded)", problem, name), true)
		return 0
	}
	w := &writer{rec: rec, at: seenAt(obj)}
	if err := w.write(name, obj); err != nil {
		rec.Error(fmt.Sprintf("amp: %v (event %s not recorded)", err, name), true)
	}
	return 0
}

// reportCounts writes one ledger.error for each failure the plugin counted.
func reportCounts(o options, stderr io.Writer) {
	if o.dropped > 0 {
		hook.WriteError(fmt.Sprintf("amp plugin: %d event(s) dropped because its queue was full; they are not recorded", o.dropped), stderr)
	}
	if o.handlerErrors > 0 {
		hook.WriteError(fmt.Sprintf("amp plugin: %d event(s) not recorded: the handler failed reading the event", o.handlerErrors), stderr)
	}
	if o.noFolder > 0 {
		hook.WriteError(fmt.Sprintf("amp plugin: %d event(s) not recorded: no folder was known for their thread "+
			"(Amp gave no workspace root and the thread ran no shell command)", o.noFolder), stderr)
	}
	if o.spillFailed > 0 {
		hook.WriteError(fmt.Sprintf("amp plugin: %d error line(s) could not be written to spill/; Amp's plugin log has them", o.spillFailed), stderr)
	}
}

// writer turns one event into its ledger lines. at is when the plugin saw the event: a thread's
// events can wait in the plugin until its folder is known, so the child's own clock may be late.
type writer struct {
	rec *hook.Recorder
	at  time.Time
}

// seenAt is the plugin's `at` (milliseconds since the epoch), when it is a plausible past time;
// otherwise zero, and the line takes this process's clock as the hook's do.
func seenAt(obj map[string]any) time.Time {
	var v struct {
		At *int64 `json:"at"`
	}
	if decode(obj, &v) != nil || v.At == nil {
		return time.Time{}
	}
	at, now := time.UnixMilli(*v.At).UTC(), clock.Now()
	if at.After(now.Add(time.Minute)) || at.Before(now.Add(-maxHeld)) {
		return time.Time{}
	}
	return at
}

func (w *writer) write(name string, obj map[string]any) error {
	switch name {
	case "session.start":
		var e sessionStart
		if err := decode(obj, &e); err != nil {
			return err
		}
		return w.sessionStarted(e)
	case "agent.start":
		var e agentStart
		if err := decode(obj, &e); err != nil {
			return err
		}
		return w.promptSubmitted(e)
	case "tool.result":
		var e toolResult
		if err := decode(obj, &e); err != nil {
			return err
		}
		return w.toolCall(e)
	case "agent.end":
		var e agentEnd
		if err := decode(obj, &e); err != nil {
			return err
		}
		return w.turnEnded(e)
	default:
		return fmt.Errorf("unknown event name %s", pytext.Repr(name))
	}
}

// decode reads the forwarded event into its typed shape. The payload reader already parsed
// it under a deadline; encoding it again is the cheapest way to a typed value.
func decode(obj map[string]any, v any) error {
	raw, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("event not re-encodable: %w", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("event not in the expected shape: %w", err)
	}
	return nil
}
