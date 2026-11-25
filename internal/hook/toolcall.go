package hook

import (
	"encoding/json"
	"math"
	"time"
)

// Claude Code's tool.call comes from its PostToolUse and PostToolUseFailure hooks, run async so
// the tool loop never waits on them. The payload says how long the call ran (duration_ms), so
// the line can place the call in time without OTEL: it ended when the hook started, and began
// duration_ms before that. Names, ids and times only; the tool's input and result are not read
// beyond the id of the subagent an Agent call ran.

// postToolUseFailure is the hook Claude Code runs after a tool call that failed.
const postToolUseFailure = "PostToolUseFailure"

// isoMillis is an instant as start_ts and end_ts carry it, to the millisecond.
func isoMillis(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000") + "+00:00" }

// claudeToolCall adds what a Claude Code tool.call line says beyond the shared fields: the
// call's id, the subagent that made it and the one it ran, whether it failed, and when it ran.
func (r *run) claudeToolCall(rec map[string]any, obj map[string]any) {
	end := r.started
	rec["source"] = "hook"
	rec["result"] = "ok"
	if obj["hook_event_name"] == postToolUseFailure {
		rec["result"] = "error"
	}
	r.payloadID(rec, obj, "tool_use_id", "tool_use_id", toolUseID.MatchString)
	r.payloadID(rec, obj, "agent_id", "agent_id", agentID.MatchString)
	if resp, ok := obj["tool_response"].(map[string]any); ok {
		r.payloadID(rec, resp, "agentId", "spawned_agent_id", agentID.MatchString)
	}
	rec["end_ts"] = isoMillis(end)
	raw, sent := obj["duration_ms"]
	if !sent {
		return // an older Claude Code sends no duration: the line still says when the call ended
	}
	n, isNum := raw.(json.Number)
	ms, err := n.Int64()
	if !isNum || err != nil || ms < 0 || ms > math.MaxInt32 {
		r.errorEvent("tool.call: the payload's duration_ms is not a whole number of milliseconds; recorded without it", true)
		return
	}
	rec["duration_ms"] = ms
	rec["start_ts"] = isoMillis(end.Add(-time.Duration(ms) * time.Millisecond))
}

// payloadID copies obj[key] to rec[field] when it is a string valid says is an id, and says so
// when it is not.
func (r *run) payloadID(rec, obj map[string]any, key, field string, valid func(string) bool) {
	s, ok := obj[key].(string)
	switch {
	case !ok || s == "":
	case valid(s):
		rec[field] = s
	default:
		r.errorEvent("tool.call: the payload's "+key+" is not a plain id; recorded without it", true)
	}
}
