package amp

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/hook"
)

// threadID is the shape of an id Livy records as a session: Amp's `T-…`, never free text.
var threadID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

var errNoThread = errors.New("no usable thread id")

// line starts an Amp line for the thread: repo and branch now, from the workspace root.
func (w *writer) line(event, thread string) (map[string]any, error) {
	if !threadID.MatchString(thread) {
		return nil, errNoThread
	}
	rec := w.rec.Event(event)
	if !w.at.IsZero() {
		rec["ts"] = clock.Seconds(w.at)
	}
	rec["source"] = "hook"
	rec["harness"] = harness
	rec["session_id"] = thread
	return rec, nil
}

func (w *writer) append(rec map[string]any) error {
	if !w.rec.Append(rec) {
		return fmt.Errorf("could not write event %v", rec["event"])
	}
	return nil
}

// sessionStarted is session.started, which Amp also fires when a thread is reopened or
// switched to: one line each time, as observed.
func (w *writer) sessionStarted(e sessionStart) error {
	rec, err := w.line("session.started", e.Thread.ID)
	if err != nil {
		return err
	}
	rec["not_observable"] = notObservable
	if v := ampVersion(); v != "" {
		rec["harness_version"] = v
	}
	switch p := e.ParentThreadID; {
	case p == "":
	case !threadID.MatchString(p) || p == e.Thread.ID:
		w.rec.Error("amp: the parent thread id is not a usable thread id; not recorded", true)
	default:
		rec["parent_session_id"] = p
	}
	return w.append(rec)
}

// promptSubmitted is prompt.submitted: the message id, never the text. A prompt Amp wrote
// itself names its workflow in reason.
func (w *writer) promptSubmitted(e agentStart) error {
	rec, err := w.line("prompt.submitted", e.Thread.ID)
	if err != nil {
		return err
	}
	if e.ID != "" {
		rec["prompt_id"] = string(e.ID)
	}
	if e.ChangesWorkflow != "" {
		rec["reason"] = e.ChangesWorkflow
	}
	return w.append(rec)
}

// toolCall is tool.call, then skill.loaded for a skill call, or work.pr_opened when the call ran
// `gh pr create` in a shell.
// There is no duration: that needs tool.call's start, which v1 does not subscribe to.
func (w *writer) toolCall(e toolResult) error {
	rec, err := w.line("tool.call", e.Thread.ID)
	if err != nil {
		return err
	}
	if e.Tool != "" {
		rec["tool"] = e.Tool
	}
	if e.ToolUseID != "" {
		rec["tool_use_id"] = e.ToolUseID
	}
	if r, ok := toolOutcome[e.Status]; ok {
		rec["result"] = r
	}
	// Amp reports a shell command that exited non-zero as done; Claude Code calls it an error.
	if e.Status == "done" && e.ExitCode != nil && *e.ExitCode != 0 {
		rec["result"] = "error"
	}
	if err := w.append(rec); err != nil {
		return err
	}
	if e.Tool == skillTool {
		return w.skillLoaded(e)
	}
	// Only a call that completed and exited 0: a gh pr create that failed opened nothing.
	failed := e.Status != "done" || (e.ExitCode != nil && *e.ExitCode != 0)
	if failed || e.Shell == nil || !hook.RunsGhPrCreate(e.Shell.Command) {
		return nil
	}
	return w.prOpened(e)
}

// prOpened is work.pr_opened for a shell `gh pr create`, its target read from the command as
// Claude Code's is: the command is rebuilt into the hook's payload shape and dropped.
func (w *writer) prOpened(e toolResult) error {
	rec, err := w.line("work.pr_opened", e.Thread.ID)
	if err != nil {
		return err
	}
	if e.Tool != "" {
		rec["tool"] = e.Tool
	}
	if e.ToolUseID != "" {
		rec["tool_use_id"] = e.ToolUseID
	}
	call := map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": e.Shell.Command}}
	if e.Shell.Dir != "" {
		call["cwd"] = e.Shell.Dir
	}
	w.rec.Target(rec, call)
	return w.append(rec)
}

// turnEnded is turn.ended, then one usage.api_call per assistant message that reports usage.
func (w *writer) turnEnded(e agentEnd) error {
	rec, err := w.line("turn.ended", e.Thread.ID)
	if err != nil {
		return err
	}
	if e.ID != "" {
		rec["prompt_id"] = string(e.ID)
	}
	if e.Status != "" {
		rec["stop_reason"] = e.Status
	}
	var errs []error
	if err := w.append(rec); err != nil {
		errs = append(errs, err)
	} else {
		noteTurn()
	}
	for i, m := range e.Messages {
		if m.Role != "assistant" || m.Usage == nil {
			continue
		}
		errs = append(errs, w.usage(e, i, m))
	}
	return errors.Join(errs...)
}

// usage is one usage.api_call. A count Amp did not report stays off the line.
func (w *writer) usage(e agentEnd, index int, m threadMessage) error {
	rec, err := w.line("usage.api_call", e.Thread.ID)
	if err != nil {
		return err
	}
	u := m.Usage
	rec["message_id"] = callMessageID(e.Thread.ID, e.ID, index, m.ID)
	if e.ID != "" {
		rec["prompt_id"] = string(e.ID)
	}
	if u.Model != "" {
		rec["model"] = u.Model
	}
	setCount(rec, "output_tokens", u.OutputTokens)
	setCount(rec, "cache_creation_input_tokens", u.CacheCreationInputTokens)
	setCount(rec, "cache_read_input_tokens", u.CacheReadInputTokens)
	if in := inputTokens(u); in != nil {
		setCount(rec, "input_tokens", in)
	}
	return w.append(rec)
}

// inputTokens is the input not read from or written to the cache, or nil when it cannot be
// known: Livy's input_tokens excludes both, whatever Amp's count includes.
func inputTokens(u *usage) *int64 {
	if u.InputTokens == nil || !inputIncludesCache {
		return u.InputTokens
	}
	in := *u.InputTokens
	for _, c := range []*int64{u.CacheCreationInputTokens, u.CacheReadInputTokens} {
		if c != nil {
			in -= *c
		}
	}
	if in < 0 {
		return nil
	}
	return &in
}

func setCount(rec map[string]any, key string, n *int64) {
	if n != nil && *n >= 0 {
		rec[key] = *n
	}
}
