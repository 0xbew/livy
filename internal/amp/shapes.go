package amp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// This file is the one place that knows Amp's payload shapes. The livy.ts plugin forwards an
// allowlisted copy of each event under Amp's own field names (no prompt, reply, tool input or
// tool output), and everything below reads it. The shapes are Amp's plugin types as
// `amp plugins show-docs` prints them, checked against events captured from Amp
// 0.0.1795017641: ids are `T-…` (thread), `M-…` (message), `TU-…` (tool use); the shell tool is
// `shell_command`, whose output carries its exitCode.

// threadRef is an event's thread: {id: ThreadID}, a `T-…` string.
type threadRef struct {
	ID string `json:"id"`
}

// sessionStart is session.start. ParentThreadID is not on Amp's event: the plugin asks
// thread.parentThreadID() and adds the answer.
type sessionStart struct {
	Thread         threadRef `json:"thread"`
	ParentThreadID string    `json:"parentThreadID"`
}

// agentStart is agent.start: the message that started the turn, and the workflow when Amp
// wrote that message itself (ship, push_branch) rather than the user.
type agentStart struct {
	Thread          threadRef `json:"thread"`
	ID              messageID `json:"id"`
	ChangesWorkflow string    `json:"changesWorkflow"`
}

// toolResult is tool.result. Three fields are not on Amp's event: ExitCode is the shell output's
// exitCode (a number, the only part of the output kept), Shell is
// amp.helpers.shellCommandFromToolCall(event), added only when its text could be `gh pr create`,
// and Skill is what a completed skill call loaded.
type toolResult struct {
	Thread    threadRef     `json:"thread"`
	ToolUseID string        `json:"toolUseID"`
	Tool      string        `json:"tool"`
	Status    string        `json:"status"`
	ExitCode  *int64        `json:"exitCode"`
	Shell     *shellCommand `json:"shell"`
	Skill     *skillFacts   `json:"skill"`
}

// skillTool is Amp's built-in tool that loads a skill: its input's name is the skill, and its
// output's text ends with "Base directory for this skill: file://<folder>".
const skillTool = "skill"

// skillFacts is a skill call's input name, and the folder from that output line as a path.
// Neither the skill's text nor the call's arguments are forwarded.
type skillFacts struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

type shellCommand struct {
	Command string `json:"command"`
	Dir     string `json:"dir"`
}

// agentEnd is agent.end: the turn's starting message id, its outcome, and the turn's
// messages, of which the plugin forwards only role, id and usage.
type agentEnd struct {
	Thread   threadRef       `json:"thread"`
	ID       messageID       `json:"id"`
	Status   string          `json:"status"`
	Messages []threadMessage `json:"messages"`
}

type threadMessage struct {
	Role  string    `json:"role"`
	ID    messageID `json:"id"`
	Usage *usage    `json:"usage"`
}

// usage is ThreadMessageUsage. The cache counts are typed `number | null`: null is "not
// reported", which is left off the line, never written as 0.
type usage struct {
	Model                    string `json:"model"`
	InputTokens              *int64 `json:"inputTokens"`
	OutputTokens             *int64 `json:"outputTokens"`
	CacheCreationInputTokens *int64 `json:"cacheCreationInputTokens"`
	CacheReadInputTokens     *int64 `json:"cacheReadInputTokens"`
}

// inputIncludesCache says Amp's inputTokens already count the cached tokens, as Codex's do and
// Claude Code's do not. Amp's do not: its thread export's totalInputTokens is inputTokens plus
// both cache counts (2 + 54,494 + 210 = 54,706).
const inputIncludesCache = false

// toolOutcome maps a tool.result status to the tool.call line's result.
var toolOutcome = map[string]string{"done": "ok", "error": "error", "cancelled": "no_result"}

// notObservable is what an Amp session.started says this capture cannot see: Amp reports no
// loaded instruction files, no model switch and no session end.
var notObservable = []any{"instructions.loaded", "model.switched", "session.ended"}

// messageID is ThreadMessageID, `number | string`, kept as its text.
type messageID string

func (m *messageID) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("message id: %w", err)
		}
		*m = messageID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New("a message id is a string or a number")
	}
	*m = messageID(n.String())
	return nil
}

// callMessageID is a usage line's message_id: Amp's own message id (`M-…`, unique across
// threads), or, for a message without one, its place in the turn, from ids Amp gave, so a
// re-sent turn dedupes.
func callMessageID(thread string, turn messageID, index int, own messageID) string {
	if own != "" {
		return string(own)
	}
	return thread + ":" + string(turn) + ":" + strconv.Itoa(index)
}
