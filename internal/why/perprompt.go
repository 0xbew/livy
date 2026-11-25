package why

import "time"

// PromptSummary is what one human prompt caused, from the ledger's text-free lines: the tools
// it ran, how they ended, what was blocked or needed approval, and the calls and tokens per
// model. Every line between a prompt and the session's next one belongs to it.
type PromptSummary struct {
	ID   string    // prompt capture's id (prompt:<transcript record uuid>)
	At   time.Time // when it was sent
	Mode string    // the permission mode it was sent in, "" when not recorded
	// Tools counts tool calls by tool.
	Tools map[string]int
	// Results counts how tool calls ended: ok and error when the prompt has tool.call lines;
	// else ran, blocked and not_run with their category (tool.outcome), plus tool_error for
	// each error a transcript records.
	Results map[string]int
	// Approvals counts tool decisions by decision and source (accept:config, reject:user_reject).
	Approvals map[string]int
	Models    map[string]Usage
}

const (
	eventDetail   = "call.detail"
	eventDecision = "tool.decision"
	kindToolError = "tool_error"
	// earlierPrompt holds a window's lines that come before its first prompt: work a prompt sent
	// before the previous commit was still doing.
	earlierPrompt = "(a prompt before this window)"
)

// summarize is one summary per prompt in a session's window, in time order, plus one for the
// lines that belong to a prompt before the window when there are any; nil when the window holds
// no prompt interaction. events are in time order.
func summarize(events []Event, in func(int) bool) []PromptSummary {
	if !hasPrompt(events, in) {
		return nil // not extracted yet: every line would read as an earlier prompt's
	}
	b := &builder{index: map[string]int{}, byMessage: map[string]int{}, byToolUse: map[string]int{}, cur: -1}
	at := make([]int, len(events)) // each line's prompt by position, -1 when not in the window
	for i, e := range events {
		at[i] = -1
		if in(i) {
			at[i] = b.add(e)
		}
	}
	for i := range b.out {
		// tool.call lines say ok or error, so a prompt that has them is told by them; a prompt
		// with none is told by tool.outcome lines. The session's tools add these up (addTools).
		if len(b.out[i].Tools) == 0 {
			b.out[i].Tools, b.out[i].Results = b.outcomes[i].Tools, b.outcomes[i].Results
		}
	}
	b.addDecisions()
	addUsage(b.out, events, at, b.byMessage)
	return b.out
}

// builder is summarize's state from one line to the next.
type builder struct {
	out       []PromptSummary
	outcomes  []PromptSummary // per summary: what tool.outcome and tool_error lines say
	index     map[string]int  // prompt id -> its summary
	byMessage map[string]int  // API message id -> the summary its call.detail names
	byToolUse map[string]int  // tool use id -> the summary its tool.call or tool.outcome is in
	decisions []placed        // tool decisions, with the summary their position gives
	cur       int             // the prompt the last line belonged to, -1 before the first
	calls     map[string]bool // tool use ids a tool.call line was counted for
}

// counted says whether a tool.call line for the call id was counted already (its hook and the
// transcript backfill both write one), and marks it counted.
func (b *builder) counted(id string) bool {
	if id == "" {
		return false
	}
	if b.calls == nil {
		b.calls = map[string]bool{}
	}
	done := b.calls[id]
	b.calls[id] = true
	return done
}

// placed is a line and the summary its position gives it.
type placed struct {
	at int
	e  Event
}

// addDecisions counts each tool decision in the summary of the tool call it decided, found by
// its tool use id; one whose call is not in the window counts where its position puts it.
func (b *builder) addDecisions() {
	for _, d := range b.decisions {
		p, ok := b.byToolUse[d.e.ToolUseID]
		if !ok || d.e.ToolUseID == "" {
			p = d.at
		}
		b.out[p].Approvals[d.e.Decision+":"+orUnknown(d.e.DecisionSource)]++
	}
}

// add takes one line of the window and returns the summary it belongs to by position.
func (b *builder) add(e Event) int {
	if e.Event == eventTurn && e.Kind == kindPrompt && e.InteractionID != "" {
		b.cur = b.summary(e)
		return -1
	}
	if b.cur < 0 {
		b.cur = b.summary(Event{InteractionID: earlierPrompt})
	}
	switch {
	case e.Event == eventDetail && e.MessageID != "":
		b.byMessage[e.MessageID] = b.detailOwner(e)
	case e.Event == eventToolCall && e.Tool != "" && !b.counted(e.ToolUseID):
		b.out[b.cur].Tools[e.Tool]++
		b.out[b.cur].Results[orUnknown(e.Result)]++
	case e.Event == eventToolResult && e.Tool != "":
		b.outcomes[b.cur].Tools[e.Tool]++
		b.outcomes[b.cur].Results[outcomeKey(e)]++
	case e.Event == eventTurn && e.Kind == kindToolError:
		b.outcomes[b.cur].Results[kindToolError]++
	case e.Event == eventDecision && e.Decision != "":
		b.decisions = append(b.decisions, placed{b.cur, e})
	}
	if e.ToolUseID != "" && (e.Event == eventToolCall || e.Event == eventToolResult) {
		b.byToolUse[e.ToolUseID] = b.cur
	}
	return b.cur
}

// summary is the index of the summary for prompt interaction e, made with its outcome side when new.
func (b *builder) summary(e Event) int {
	p := promptAt(&b.out, b.index, e)
	for len(b.outcomes) < len(b.out) {
		b.outcomes = append(b.outcomes, PromptSummary{Tools: map[string]int{}, Results: map[string]int{}})
	}
	return p
}

// detailOwner is the summary a call.detail line's API call belongs to: the prompt it names, the
// earlier-prompt summary when it names one outside the window, or the prompt before it.
func (b *builder) detailOwner(e Event) int {
	if p, ok := b.index[e.PromptInteractionID]; ok {
		return p
	}
	if e.PromptInteractionID != "" {
		return b.summary(Event{InteractionID: earlierPrompt})
	}
	return b.cur
}

// promptAt is the index of the summary for prompt interaction e, made when it is new: a prompt
// extracted twice is one prompt.
func promptAt(out *[]PromptSummary, index map[string]int, e Event) int {
	if i, ok := index[e.InteractionID]; ok {
		return i
	}
	t, _ := parseTS(e.TS)
	*out = append(*out, PromptSummary{ID: e.InteractionID, At: t, Mode: e.PermissionMode,
		Tools: map[string]int{}, Results: map[string]int{}, Approvals: map[string]int{}, Models: map[string]Usage{}})
	index[e.InteractionID] = len(*out) - 1
	return len(*out) - 1
}

// hasPrompt is whether the window has a prompt interaction line.
func hasPrompt(events []Event, in func(int) bool) bool {
	for i, e := range events {
		if e.Event == eventTurn && e.Kind == kindPrompt && e.InteractionID != "" && in(i) {
			return true
		}
	}
	return false
}

func outcomeKey(e Event) string {
	if e.BlockCategory != "" {
		return orUnknown(e.Outcome) + ":" + e.BlockCategory
	}
	return orUnknown(e.Outcome)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// addUsage adds each API call once to the prompt its message's call.detail names, else to the
// prompt before it.
func addUsage(out []PromptSummary, events []Event, at []int, byMessage map[string]int) {
	seen := map[[2]string]bool{}
	for i, e := range events {
		if e.Event != eventUsage || at[i] < 0 || !firstSeen(seen, e) {
			continue
		}
		p, named := byMessage[e.MessageID]
		if !named {
			p = at[i]
		}
		addCall(out[p].Models, e)
	}
}
