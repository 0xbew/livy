package hook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/0xbew/livy/internal/pytext"
)

// `livy hook --harness codex <event>` records Codex's hooks (Codex 0.171 and later), which
// send Claude Code's payload shape with Codex's extensions: turn_id on every turn-scoped
// event, and agent_id and agent_type on a subagent's. Lines carry harness "codex".
//
// Codex has no prompt id: its turn id is the prompt's, and is recorded as both. A
// subagent's own turns are not a person's prompt, so its UserPromptSubmit (the brief) writes
// nothing and its lines carry agent_id and subagent_type, never a prompt_id.
const harnessCodex = "codex"

// codexHooks is, per event Codex's hooks write, the Codex hook that sends it.
var codexHooks = map[string]string{"session.started": "SessionStart", "prompt.submitted": "UserPromptSubmit",
	"tool.call": "PostToolUse", "agent.launched": "SubagentStart", "turn.ended": "Stop", "session.ended": "SessionEnd"}

// codexNotObservable is what a Codex session.started says this capture cannot see: Codex
// reports no loaded instruction file, no model switch (each line names its model) and no
// skill load (a skill is read through the shell).
var codexNotObservable = []any{"instructions.loaded", "model.switched", "skill.loaded"}

// toolUseID is the shape of a tool call id Livy records: Codex's `exec-…` and `call_…`.
var toolUseID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// codexShell is the tool name Codex's hooks give a shell call.
const codexShell = "Bash"

// rolloutTail is how much of a rollout's end is read for a shell call's exit code: the
// call completed just before its PostToolUse ran.
const rolloutTail = 8 << 20

// codexPayload is the part of a Codex hook payload Livy reads. tool_input and cwd are read
// by the shared target code from the object itself.
type codexPayload struct {
	HookEventName  string  `json:"hook_event_name"`
	SessionID      string  `json:"session_id"`
	TurnID         string  `json:"turn_id"`
	AgentID        string  `json:"agent_id"`
	AgentType      string  `json:"agent_type"`
	ToolName       string  `json:"tool_name"`
	ToolUseID      string  `json:"tool_use_id"`
	TranscriptPath *string `json:"transcript_path"`
}

// codexEvent writes the line for one Codex hook, or a ledger.error saying why it cannot.
func (r *run) codexEvent(name string, overrides map[string]string, p payload) {
	if ignored := ignoredOverrides(overrides); len(ignored) > 0 {
		r.errorEvent(fmt.Sprintf("ignored override(s) %s (event %s still recorded)", strings.Join(ignored, ", "), name), true)
	}
	hookName, known := codexHooks[name]
	if !known {
		r.errorEvent("codex: no Codex hook writes "+name+"; not recorded", true)
		return
	}
	e, problem := readCodexPayload(p, hookName)
	if problem != "" {
		r.errorEvent(fmt.Sprintf("codex: %s (event %s not recorded)", problem, name), true)
		return
	}
	if name == "prompt.submitted" && e.AgentID != "" {
		return // a subagent's brief: SubagentStart already recorded the launch
	}
	rec := r.build(name, overrides, p)
	r.codexFields(rec, name, e)
	if !r.append(rec) {
		r.errorEvent("could not write event "+name, true)
		return
	}
	if name == "tool.call" && e.ToolName == codexShell {
		r.codexPrOpened(overrides, p, e)
	}
}

// readCodexPayload is the payload's typed fields; problem says why it cannot be used: none
// came, it is not JSON, it is from another hook than the one wired to this event, or it has
// no session id. Each is a change in Codex or in the hooks file, never a guess.
func readCodexPayload(p payload, hookName string) (codexPayload, string) {
	if p.problem != "" {
		return codexPayload{}, p.problem
	}
	if p.obj == nil {
		return codexPayload{}, "no payload on stdin"
	}
	raw, err := json.Marshal(p.obj)
	if err != nil {
		return codexPayload{}, "payload not re-encodable"
	}
	var e codexPayload
	if err := json.Unmarshal(raw, &e); err != nil {
		return codexPayload{}, "payload not in Codex's hook shape (" + err.Error() + ")"
	}
	switch {
	case e.HookEventName != hookName:
		return codexPayload{}, fmt.Sprintf("hook_event_name %s, want %s", pytext.Repr(e.HookEventName), hookName)
	case !sessionID.MatchString(e.SessionID):
		return codexPayload{}, "no usable session_id"
	}
	return e, ""
}

// codexFields adds what Codex's payload says beyond Claude Code's: the harness, the turn
// (and, outside a subagent, the prompt it answers), the subagent, the tool call's id.
func (r *run) codexFields(rec map[string]any, name string, e codexPayload) {
	rec["harness"] = harnessCodex
	if e.TurnID != "" && sessionID.MatchString(e.TurnID) {
		rec["turn_id"] = e.TurnID
		if e.AgentID == "" {
			rec["prompt_id"] = e.TurnID
		}
	}
	r.codexAgent(rec, e)
	switch name {
	case "session.started":
		rec["not_observable"] = codexNotObservable
		if v := codexVersion(e.TranscriptPath); v != "" {
			rec["harness_version"] = v
		}
	case "tool.call":
		rec["source"] = "hook"
		r.codexToolUseID(rec, e)
	case "agent.launched":
		rec["source"] = "hook"
		rec["agent_label_status"] = "absent" // Codex gives a subagent no label, only its brief
	}
}

// codexAgent adds the subagent a line is from, each value checked as the launcher tags are.
func (r *run) codexAgent(rec map[string]any, e codexPayload) {
	switch {
	case e.AgentID == "":
	case agentID.MatchString(e.AgentID):
		rec["agent_id"] = e.AgentID
	default:
		r.errorEvent("codex: the payload's agent_id is not an agent id; recorded without it", true)
	}
	switch {
	case e.AgentType == "":
	case roleToken.MatchString(e.AgentType):
		rec["subagent_type"] = e.AgentType
	default:
		r.errorEvent("codex: the payload's agent_type is not a plain token; recorded without it", true)
	}
}

func (r *run) codexToolUseID(rec map[string]any, e codexPayload) {
	switch {
	case e.ToolUseID == "":
	case toolUseID.MatchString(e.ToolUseID):
		rec["tool_use_id"] = e.ToolUseID
	default:
		r.errorEvent("codex: the payload's tool_use_id is not a plain id; recorded without it", true)
	}
}

// codexPrOpened writes work.pr_opened for a shell call that ran `gh pr create` and exited 0.
// Codex runs PostToolUse whatever the exit code and its payload has none, so the code is read
// from the call's completion record in the rollout; one that cannot be found is said.
func (r *run) codexPrOpened(overrides map[string]string, p payload, e codexPayload) {
	input, _ := p.obj["tool_input"].(map[string]any)
	command, _ := input["command"].(string)
	if !RunsGhPrCreate(command) {
		return
	}
	code, found := shellExitCode(e.TranscriptPath, e.ToolUseID)
	switch {
	case !found:
		r.errorEvent("codex: work.pr_opened not recorded: the gh pr create call's exit code is not in its rollout", true)
		return
	case code != 0:
		return // a gh pr create that failed opened nothing
	}
	rec := r.build("work.pr_opened", overrides, p)
	r.codexFields(rec, "work.pr_opened", e)
	r.codexToolUseID(rec, e)
	r.target(rec, p.obj)
	if !r.append(rec) {
		r.errorEvent("could not write event work.pr_opened", true)
	}
}

// rolloutRecord is the part of a rollout line that reports a finished shell call.
type rolloutRecord struct {
	Type    string `json:"type"`
	Payload struct {
		Type string `json:"type"`
		Item struct {
			ID       string `json:"id"`
			ExitCode *int   `json:"exit_code"`
		} `json:"item"`
	} `json:"payload"`
}

// shellExitCode is the exit code the rollout's item_completed record gives the call, newest
// first; found is false when the rollout names none.
func shellExitCode(path *string, callID string) (code int, found bool) {
	if path == nil || callID == "" {
		return 0, false
	}
	buf := readTail(*path, rolloutTail)
	lines := bytes.Split(buf, []byte("\n"))
	for _, line := range slices.Backward(lines) {
		if !bytes.Contains(line, []byte(callID)) {
			continue
		}
		var rec rolloutRecord
		if json.Unmarshal(line, &rec) != nil || rec.Payload.Type != "item_completed" || rec.Payload.Item.ID != callID {
			continue
		}
		if rec.Payload.Item.ExitCode != nil {
			return *rec.Payload.Item.ExitCode, true
		}
	}
	return 0, false
}

// readTail is the last n bytes of the file at path, nil when it cannot be read.
func readTail(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }() // opened read-only: a close error loses nothing
	if st, err := f.Stat(); err == nil && st.Size() > n {
		if _, err := f.Seek(-n, io.SeekEnd); err != nil {
			return nil
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	return b
}

// sessionMeta is the part of a rollout's first line that names the Codex version.
type sessionMeta struct {
	Type    string `json:"type"`
	Payload struct {
		CLIVersion string `json:"cli_version"`
	} `json:"payload"`
}

// codexVersion is the Codex version the rollout's session_meta names, or "" when it cannot
// be read: the version is the Agent-Harness trailer's, never a guess.
func codexVersion(path *string) string {
	if path == nil {
		return ""
	}
	f, err := os.Open(*path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }() // opened read-only: a close error loses nothing
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	if !sc.Scan() {
		return ""
	}
	var m sessionMeta
	if json.Unmarshal(sc.Bytes(), &m) != nil || m.Type != "session_meta" || !roleToken.MatchString(m.Payload.CLIVersion) {
		return ""
	}
	return m.Payload.CLIVersion
}
