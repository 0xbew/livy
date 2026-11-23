package spotcheck

import (
	"fmt"
	"io"
	"os"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/denylist"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/pytext"
)

// Main is `livy spotcheck`, the async PreToolUse hook. It returns the exit code, always 0,
// and writes nothing to stdout in hook mode: a hook's output reaches Claude Code.
func Main(args []string, stdin *os.File, stdout, stderr io.Writer) (code int) {
	if len(args) > 0 && args[0] == "--selftest" {
		return Selftest(stdout, stderr, func() bool { return key() != "" })
	}
	var rec *hook.Recorder
	defer func() {
		if p := recover(); p != nil {
			code = 0
			defer func() { _ = recover() }()
			if rec != nil {
				rec.Error(fmt.Sprintf("spotcheck unhandled panic: %T", p), true)
			}
		}
	}()
	// The key first: without one this is a no-op that touches neither stdin nor git.
	k := key()
	if k == "" {
		return 0
	}
	rec = hook.NewRecorder(stderr)
	if !rec.InScope() {
		if _, timedOut := rec.GitTimeout(); timedOut {
			rec.GitTimedOutError("spotcheck: ", "check not run")
		}
		return 0
	}
	obj, problem := rec.Payload(stdin)
	if problem != "" {
		rec.Error("spotcheck: "+problem, true)
		return 0
	}
	if len(obj) > 0 {
		check(rec, obj, k)
	}
	return 0
}

func key() string {
	dirs := []string{hook.DataDir()}
	if legacy := hook.LegacyDir(); legacy != "" && legacy != dirs[0] {
		dirs = append(dirs, legacy)
	}
	return APIKey(os.Getenv, dirs...)
}

func denylistPath() string { return denylist.Path(hook.Settings(), hook.DataDir()) }

// check plans, decides and records one tool call's check.decision.
func check(c *hook.Recorder, obj map[string]any, key string) {
	dl := LoadDenylist(denylistPath())
	planned := Plan(obj, dl.D, c.Slug())
	if planned == nil {
		return
	}
	rec := c.Event("check.decision")
	rec["check"], rec["contract"] = planned.Check, planned.Contract
	for _, kv := range [][2]string{{"session_id", "session_id"}, {"tool_use_id", "tool_use_id"}, {"tool_name", "tool"}} {
		if s, ok := obj[kv[0]].(string); ok && pytext.Strip(s) != "" {
			rec[kv[1]] = pytext.Strip(s)
		}
	}
	if planned.SubagentType != nil {
		rec["subagent_type"] = *planned.SubagentType
	}
	if planned.OpaqueScript != nil {
		rec["opaque_script"] = *planned.OpaqueScript
	}
	if dl.Version != nil {
		rec["denylist_version"] = int64(*dl.Version)
	}
	if planned.State != nil {
		rec["input_sha"] = InputSHA(planned.State)
	}
	c.Target(rec, obj)
	got := NewAsker().Decide(planned, key, dl, hook.DataDir(), clock.Now())
	if got == nil {
		return // paused; the pause was recorded once, when it began
	}
	for k, v := range got {
		if scores, ok := v.(map[string]float64); ok {
			m := map[string]any{}
			for name, f := range scores {
				m[name] = f
			}
			v = m
		}
		rec[k] = v
	}
	if !c.Append(rec) {
		c.Error(fmt.Sprintf("spotcheck: could not write a %v receipt", rec["verdict"]), true)
	}
}
