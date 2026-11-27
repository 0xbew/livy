// Package why is `livy why <sha|range>`: for each commit, the agent sessions behind it and,
// per session, the prompts since the previous commit in that session and branch (for its first
// commit on a branch, since the branch's checkout or its previous commit anywhere), the tools
// they ran, the models and tokens, and the ticket key. Text-free: it reads ids, names and
// counts from trailers and the ledger, never prompt text.
package why

import (
	"maps"
	"slices"
	"time"
)

// Commit is what git says about one commit.
type Commit struct {
	SHA       string
	Committed time.Time
	Sessions  []string // Agent-Session trailer values
	Harness   string   // Agent-Harness trailer
	Model     string   // Agent-Model trailer
	Parents   int      // how many parents it has: 2 or more for a merge commit
	// Ancestors are earlier commits on its first-parent line, newest first, with their
	// Agent-Session trailers and commit time: where the previous commit of a session is found
	// when the ledger has no commit.recorded for it.
	Ancestors []Commit
	// AncestorProblem is why Ancestors could not be read, "" when they were.
	AncestorProblem string
}

// Event is the part of a ledger line why reads; every other field is ignored.
type Event struct {
	Event               string `json:"event"`
	TS                  string `json:"ts"`
	SessionID           string `json:"session_id"`
	PromptID            string `json:"prompt_id"`
	Repo                string `json:"repo"`
	Branch              string `json:"branch"`
	Harness             string `json:"harness"`
	Model               string `json:"model"`
	Tool                string `json:"tool"`
	Result              string `json:"result"`
	Outcome             string `json:"outcome"`
	MessageID           string `json:"message_id"`
	RequestID           string `json:"request_id"`
	CommitSHA           string `json:"commit_sha"`
	OldCommitSHA        string `json:"old_commit_sha"`
	RewriteKind         string `json:"rewrite_kind"`
	TicketKey           string `json:"ticket_key"`
	Kind                string `json:"kind"`
	InteractionID       string `json:"interaction_id"`
	PromptInteractionID string `json:"prompt_interaction_id"`
	ParentSessionID     string `json:"parent_session_id"`
	PermissionMode      string `json:"permission_mode"`
	BlockCategory       string `json:"block_category"`
	Decision            string `json:"decision"`
	DecisionSource      string `json:"decision_source"`
	ToolUseID           string `json:"tool_use_id"`
	// A tool.call line's times, to the millisecond, and the subagent an Agent call ran.
	StartTS        string `json:"start_ts"`
	EndTS          string `json:"end_ts"`
	SpawnedAgentID string `json:"spawned_agent_id"`
	// A call.detail line's tools: the tool calls that API message asked for, by name.
	Tools               []string `json:"tools"`
	Source              string   `json:"source"`
	AgentID             string   `json:"agent_id"`
	SubagentType        string   `json:"subagent_type"`
	InputTokens         int64    `json:"input_tokens"`
	OutputTokens        int64    `json:"output_tokens"`
	CacheReadTokens     int64    `json:"cache_read_input_tokens"`
	CacheCreationTokens int64    `json:"cache_creation_input_tokens"`
	// The cache writes split by lifetime; nil when the line does not split them (Amp, Codex).
	CacheCreation5mTokens *int64 `json:"cache_creation_5m_input_tokens"`
	CacheCreation1hTokens *int64 `json:"cache_creation_1h_input_tokens"`
	ServiceTier           string `json:"service_tier"`
	Speed                 string `json:"speed"`
	// A skill.loaded line's skill, as `livy pr` lists the skills a window loaded.
	Skill      string `json:"skill"`
	Scope      string `json:"scope"`
	Trigger    string `json:"trigger"`
	SHA256     string `json:"sha256"`
	TreeSHA256 string `json:"tree_sha256"`
	// A call.detail line's stop reason: "end_turn" when the model handed back to the human.
	StopReason string `json:"stop_reason"`
	// A skills.listed line's skills: those Claude Code listed to the model as available.
	ListedSkills []ListedSkill `json:"listed_skills"`
}

// ListedSkill is one skill a skills.listed line names, with its digest ("" when none was written).
type ListedSkill struct {
	Skill  string `json:"skill"`
	Digest string `json:"digest"`
}

// Report is what why says about one commit.
type Report struct {
	SHA      string
	Sessions []Session
}

// Session is one session's work behind a commit.
type Session struct {
	ID       string
	Harness  string
	From, To time.Time
	Basis    string // how From was found
	Prompts  []string
	Tools    map[string]int
	Models   map[string]Usage
	// Calls are the window's API calls, each once, in time order: what `livy pr` prices.
	Calls  []Call
	Ticket string
	// PerPrompt is what each prompt in the window caused, in time order.
	PerPrompt []PromptSummary
	// Sent is when each of Prompts was first sent, in the same order; zero when no line says.
	// `livy profile` times a window from these, so both commands count the same prompts.
	Sent []time.Time
	// AgentBriefs is the window's agent-written brief, kept out of Prompts: the opening prompt of
	// a session another session launched. BriefSent is when it was sent, as Sent is for Prompts.
	AgentBriefs []string
	BriefSent   []time.Time
}

// Usage is one model's calls and tokens.
type Usage struct{ Calls, Input, Output, CacheRead, CacheWrite int64 }

// Call is one usage.api_call line in a window: its tokens, and what decides its price.
type Call struct {
	At                    time.Time
	Model                 string
	AgentID               string
	MessageID, RequestID  string
	ServiceTier, Speed    string
	Input, Output         int64
	CacheRead, CacheWrite int64
	// CacheWrite5m and CacheWrite1h split CacheWrite by lifetime; nil when the line does not.
	CacheWrite5m, CacheWrite1h *int64
}

// Basis values: how a window's start was found.
const (
	basisRecorded   = "commit.recorded "
	basisTrailer    = "Agent-Session trailer on "
	basisNoEarlier  = "no earlier commit in this session"
	basisCheckout   = "work.started: checkout of "
	basisAnyBranch  = "the session's previous commit on any branch: commit.recorded "
	shortSHA        = 12
	eventRecorded   = "commit.recorded"
	eventRewritten  = "commit.rewritten"
	eventSubmitted  = "prompt.submitted"
	eventSession    = "session.started"
	eventCaptured   = "prompt.captured"
	eventStarted    = "work.started"
	eventToolCall   = "tool.call"
	eventToolResult = "tool.outcome"
	eventUsage      = "usage.api_call"
	rewriteRebase   = "rebase"
	eventTurn       = "interaction"
	kindPrompt      = "prompt"
	// typedWithin is how far a prompt.submitted line may sit from the prompt interaction line
	// extraction writes for the same prompt. On this machine's ledger the hook line and the
	// transcript record are 0-1 s apart, and the nearest hand-back or notice to a typed prompt 11 s.
	typedWithin = 5 * time.Second
)

// Explain is the report for c from the ledger events: one Session per session that made it,
// the one its commit.recorded names first, then those its Agent-Session trailers name.
func Explain(c Commit, events []Event) Report {
	events = byInstant(events)
	aliases := aliasesOf(c.SHA, events)
	replays := replaysIn(events)
	at := recordedFor(aliases, replays, events)
	// Neither the commit's own SHAs nor a replay can be the session's previous commit.
	notPrevious := maps.Clone(replays)
	for _, a := range aliases {
		notPrevious[a] = true
	}
	var rec *Event
	if at >= 0 {
		rec = &events[at]
	}
	var ids []string
	if rec != nil && rec.SessionID != "" {
		ids = append(ids, rec.SessionID)
	}
	for _, s := range c.Sessions {
		if s != "" && !slices.Contains(ids, s) {
			ids = append(ids, s)
		}
	}
	r := Report{SHA: c.SHA}
	for _, id := range ids {
		r.Sessions = append(r.Sessions, explainSession(c, id, at, notPrevious, events))
	}
	return r
}

// aliasesOf is sha and every SHA it was rewritten from (amend, rebase), newest first.
func aliasesOf(sha string, events []Event) []string {
	out := []string{sha}
	for i := 0; i < len(out); i++ {
		for _, e := range events {
			if e.Event == eventRewritten && e.CommitSHA == out[i] && e.OldCommitSHA != "" && !slices.Contains(out, e.OldCommitSHA) {
				out = append(out, e.OldCommitSHA)
			}
		}
	}
	return out
}

// replaysIn is the picks a rebase replayed: the new side of each rebase commit.rewritten line.
// post-commit records them like any commit, at the moment of the rebase; the work behind one is
// the commit it replays.
func replaysIn(events []Event) map[string]bool {
	out := map[string]bool{}
	for _, e := range events {
		if e.Event == eventRewritten && e.RewriteKind == rewriteRebase && e.CommitSHA != "" {
			out[e.CommitSHA] = true
		}
	}
	return out
}

// recordedFor is the index of the commit.recorded line of the newest alias that has one, -1
// when none does.
func recordedFor(aliases []string, replays map[string]bool, events []Event) int {
	for _, a := range aliases {
		found := -1
		for i := range events {
			if events[i].Event == eventRecorded && events[i].CommitSHA == a && !replays[a] {
				found = i
			}
		}
		if found >= 0 {
			return found
		}
	}
	return -1
}

// explainSession is session id's work behind c; at is the index of c's commit.recorded line
// in events, -1 when it has none.
func explainSession(c Commit, id string, at int, notPrevious map[string]bool, events []Event) Session {
	s := Session{ID: id, Harness: c.Harness, To: c.Committed.UTC(), Tools: map[string]int{}, Models: map[string]Usage{}}
	var rec *Event
	if at >= 0 && events[at].SessionID == id {
		rec = &events[at]
	}
	branch := ""
	if rec != nil {
		if t, ok := parseTS(rec.TS); ok {
			s.To = t
		}
		s.Harness, branch, s.Ticket = rec.Harness, rec.Branch, rec.TicketKey
	}
	prev := -1
	if rec != nil {
		// Only the commit's own record names its branch; with none, its history decides.
		s.From, prev, s.Basis = previousRecorded(s, branch, notPrevious, events[:at])
	}
	if prev < 0 {
		before := events
		if rec != nil {
			before = events[:at]
		}
		s.From, s.Basis = firstOnBranch(c, s, rec, notPrevious, before)
	}
	end := -1
	if rec != nil {
		end = at
	}
	in := window{events: events, id: id, from: s.From, to: s.To, prev: prev, end: end}.has
	untyped := notTyped(events, id)
	typed := func(i int) bool { return in(i) && (events[i].Event != eventSubmitted || !untyped[events[i].PromptID]) }
	// The hook's prompt ids first (Claude Code's own, as OTEL carries them), then the ids
	// extraction gives prompts it read from transcripts and rollouts.
	for _, kind := range []string{eventSubmitted, eventCaptured, eventTurn} {
		if s.Prompts, s.Sent = promptsIn(events, typed, kind); len(s.Prompts) > 0 {
			break
		}
	}
	if rec != nil && rec.PromptID != "" && !untyped[rec.PromptID] && !slices.Contains(s.Prompts, rec.PromptID) {
		s.Prompts = append(s.Prompts, rec.PromptID)
		s.Sent = append(s.Sent, firstSent(events, rec.PromptID))
	}
	s.splitBriefs(agentBriefs(events, id))
	s.Calls = countUsage(s.Models, events, in)
	s.PerPrompt = summarize(events, in)
	addTools(s.Tools, s.PerPrompt, events, in)
	if s.Ticket == "" {
		s.Ticket = newestTicket(events, in)
	}
	return s
}

// agentBriefs is the ids a session's opening prompt goes by, one per kind of prompt line, when
// another session launched it (its session.started line names a parent session): a launcher's
// agent wrote that brief, not a person. Nil for any other session. The role tag alone does not
// tell: a person's own top-level session can carry one too.
func agentBriefs(events []Event, id string) map[string]bool {
	launched := slices.ContainsFunc(events, func(e Event) bool {
		return e.Event == eventSession && e.SessionID == id && e.ParentSessionID != ""
	})
	if !launched {
		return nil
	}
	ofSession := func(i int) bool { return events[i].SessionID == id }
	var first []string
	var at []time.Time
	for _, kind := range []string{eventSubmitted, eventCaptured, eventTurn} {
		if ids, sent := promptsIn(events, ofSession, kind); len(ids) > 0 {
			first, at = append(first, ids[0]), append(at, sent[0])
		}
	}
	// Each kind's first line is the brief only when it is the session's first prompt by any
	// kind: a kind that missed the brief would otherwise class a later prompt as one.
	if len(at) == 0 {
		return nil
	}
	opened := slices.MinFunc(at, time.Time.Compare)
	out := map[string]bool{}
	for i, t := range at {
		if t.Sub(opened) <= typedWithin {
			out[first[i]] = true
		}
	}
	return out
}

// splitBriefs moves the prompts briefs names from Prompts to AgentBriefs, with their send times.
func (s *Session) splitBriefs(briefs map[string]bool) {
	if len(briefs) == 0 {
		return
	}
	var ids []string
	var sent []time.Time
	for i, id := range s.Prompts {
		if briefs[id] {
			s.AgentBriefs, s.BriefSent = append(s.AgentBriefs, id), append(s.BriefSent, s.Sent[i])
			continue
		}
		ids, sent = append(ids, id), append(sent, s.Sent[i])
	}
	s.Prompts, s.Sent = ids, sent
}

// window is one session's events behind a commit: after from (or, when prev is set, after the
// previous commit's line) and up to to (or, when end is set, up to the commit's own line).
// Between two commit.recorded lines it goes by ledger position: git and the ledger keep whole
// seconds, so two commits in one second share a timestamp.
type window struct {
	events    []Event
	id        string
	from, to  time.Time
	prev, end int // -1 when unset
}

func (w window) has(i int) bool {
	e := w.events[i]
	t, ok := parseTS(e.TS)
	if !ok || e.SessionID != w.id {
		return false
	}
	after, upTo := t.After(w.from), !t.After(w.to)
	if w.prev >= 0 {
		after = i > w.prev
	}
	if w.end >= 0 {
		upTo = i <= w.end
	}
	return after && upTo
}

// previousRecorded is the session's last commit.recorded line on branch among before (the
// lines ahead of the commit's own), not one notPrevious names (the commit's own SHAs and
// replayed picks): when it was made, its index, and how it was found. Index -1 when there is
// none.
func previousRecorded(s Session, branch string, notPrevious map[string]bool, before []Event) (time.Time, int, string) {
	for i, e := range slices.Backward(before) {
		if e.Event != eventRecorded || e.SessionID != s.ID || e.Branch != branch || notPrevious[e.CommitSHA] {
			continue
		}
		if t, ok := parseTS(e.TS); ok {
			return t, i, basisRecorded + short(e.CommitSHA)
		}
	}
	return time.Time{}, -1, ""
}

// previousByTrailer is when the session's previous commit on c's first-parent line was made,
// skipping the ones notPrevious names (replayed picks), and how it was found; zero when there
// is none.
func previousByTrailer(c Commit, s Session, notPrevious map[string]bool) (time.Time, string) {
	id, to := s.ID, s.To
	// Not after the end, rather than before it: git keeps whole seconds. An ancestor made later
	// than the commit was recorded is one a rebase put under it, not its predecessor.
	for _, a := range c.Ancestors {
		if slices.Contains(a.Sessions, id) && !a.Committed.After(to) && !notPrevious[a.SHA] {
			return a.Committed.UTC(), basisTrailer + short(a.SHA)
		}
	}
	return time.Time{}, basisNoEarlier
}

// firstOnBranch is where the window of a commit with no earlier commit by its session on its
// branch starts, and how it was found: the session's previous commit by its first-parent
// trailers or the branch's checkout, whichever is later; with neither, the session's previous
// commit on any branch; with none, the session's start. Without the last two steps, a session
// that commits on several branches credits all it did since it started to each branch's first
// commit. before is the lines ahead of the commit's own commit.recorded, or every line when it has none.
func firstOnBranch(c Commit, s Session, rec *Event, notPrevious map[string]bool, before []Event) (time.Time, string) {
	from, basis := previousByTrailer(c, s, notPrevious)
	if rec != nil {
		if t, ok := checkedOut(rec.Repo, rec.Branch, before); ok && t.After(from) {
			from, basis = t, basisCheckout+rec.Branch
		}
	}
	if !from.IsZero() {
		return from, basis
	}
	for _, e := range slices.Backward(before) {
		if e.Event != eventRecorded || e.SessionID != s.ID || notPrevious[e.CommitSHA] {
			continue
		}
		if t, ok := parseTS(e.TS); ok && !t.After(s.To) {
			return t, basisAnyBranch + short(e.CommitSHA)
		}
	}
	return from, basis
}

// checkedOut is when branch was last checked out in repo (its newest work.started line) among
// before. The git hook that writes the line knows no session, so it is the branch's start for
// whichever session works on it.
func checkedOut(repo, branch string, before []Event) (time.Time, bool) {
	if repo == "" || branch == "" {
		return time.Time{}, false
	}
	for _, e := range slices.Backward(before) {
		if e.Event == eventStarted && e.Repo == repo && e.Branch == branch {
			if t, ok := parseTS(e.TS); ok {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// promptsIn is the ids of the window's prompts that kind of line names, in time order, and when
// each was first sent.
func promptsIn(events []Event, in func(int) bool, kind string) (ids []string, sent []time.Time) {
	for i, e := range events {
		id := e.PromptID
		if kind == eventTurn {
			id = ""
			if e.Kind == kindPrompt {
				id = e.InteractionID
			}
		}
		if e.Event == kind && id != "" && in(i) && !slices.Contains(ids, id) {
			t, _ := parseTS(e.TS) // in() has parsed it
			ids, sent = append(ids, id), append(sent, t)
		}
	}
	return ids, sent
}

// notTyped is the ids of session id's prompt.submitted lines that no person typed. Claude Code's
// UserPromptSubmit hook also fires for a subagent's hand-back, a task notification and a
// scheduled or meta prompt; extraction writes a prompt interaction line only for a typed prompt.
// So a prompt.submitted line extraction has read past (the session has a later interaction or
// call.detail line) with no prompt interaction line within typedWithin was not typed. One
// extraction has not reached yet counts, as does every one in a session it never reads (Codex).
func notTyped(events []Event, id string) map[string]bool {
	typedAt, read := extracted(events, id)
	out, typed := map[string]bool{}, map[string]bool{}
	for _, e := range events {
		if e.Event != eventSubmitted || e.SessionID != id || e.PromptID == "" {
			continue
		}
		t, ok := parseTS(e.TS)
		if !ok || !t.Before(read) {
			continue
		}
		if slices.ContainsFunc(typedAt, func(at time.Time) bool { return t.Sub(at).Abs() <= typedWithin }) {
			typed[e.PromptID] = true
		} else {
			out[e.PromptID] = true
		}
	}
	for p := range typed {
		delete(out, p)
	}
	return out
}

// extracted is when session id's prompt interaction lines were sent, and its newest line
// extraction wrote (interaction or call.detail): how far extraction has read.
func extracted(events []Event, id string) (typedAt []time.Time, read time.Time) {
	for _, e := range events {
		if e.SessionID != id || (e.Event != eventTurn && e.Event != eventDetail) {
			continue
		}
		t, ok := parseTS(e.TS)
		if !ok {
			continue
		}
		if e.Event == eventTurn && e.Kind == kindPrompt {
			typedAt = append(typedAt, t)
		}
		if t.After(read) {
			read = t
		}
	}
	return typedAt, read
}

// firstSent is when the first prompt.submitted line for id was written, zero when none is.
func firstSent(events []Event, id string) time.Time {
	for _, e := range events {
		if e.Event == eventSubmitted && e.PromptID == id {
			if t, ok := parseTS(e.TS); ok {
				return t
			}
		}
	}
	return time.Time{}
}

// addTools counts the window's tool calls by tool. A window with prompt lines adds up its
// prompts' counts, each of which chose its own source, so the session's total and its
// per-prompt lines agree when only some prompts have tool.call lines.
func addTools(into map[string]int, perPrompt []PromptSummary, events []Event, in func(int) bool) {
	if len(perPrompt) == 0 {
		countTools(into, events, in)
		return
	}
	for _, p := range perPrompt {
		for tool, n := range p.Tools {
			into[tool] += n
		}
	}
}

// countTools counts the tool calls by tool: tool.call lines, else tool.outcome lines. A call
// both its hook and the transcript backfill wrote a line for counts once.
func countTools(into map[string]int, events []Event, in func(int) bool) {
	for _, kind := range []string{eventToolCall, eventToolResult} {
		seen := map[string]bool{}
		for i, e := range events {
			if e.Event != kind || e.Tool == "" || !in(i) || (e.ToolUseID != "" && seen[e.ToolUseID]) {
				continue
			}
			seen[e.ToolUseID] = true
			into[e.Tool]++
		}
		if len(into) > 0 {
			return
		}
	}
}

// countUsage sums tokens per model, once per API call (a call extracted twice has the same
// message and request id), and returns the calls counted.
func countUsage(into map[string]Usage, events []Event, in func(int) bool) []Call {
	seen := map[[2]string]bool{}
	var calls []Call
	for i, e := range events {
		if e.Event == eventUsage && in(i) && firstSeen(seen, e) {
			addCall(into, e)
			t, _ := parseTS(e.TS) // in() has parsed it
			calls = append(calls, Call{At: t, Model: e.Model, AgentID: e.AgentID, MessageID: e.MessageID,
				RequestID: e.RequestID, ServiceTier: e.ServiceTier, Speed: e.Speed, Input: e.InputTokens,
				Output: e.OutputTokens, CacheRead: e.CacheReadTokens, CacheWrite: e.CacheCreationTokens,
				CacheWrite5m: e.CacheCreation5mTokens, CacheWrite1h: e.CacheCreation1hTokens})
		}
	}
	return calls
}

// firstSeen is false for an API call already counted: one extracted twice has the same message
// and request id.
func firstSeen(seen map[[2]string]bool, e Event) bool {
	key := [2]string{e.MessageID, e.RequestID}
	if key == [2]string{} {
		return true
	}
	if seen[key] {
		return false
	}
	seen[key] = true
	return true
}

// addCall adds one usage.api_call line to its model's calls and tokens.
func addCall(into map[string]Usage, e Event) {
	u := into[e.Model]
	u.Calls++
	u.Input += e.InputTokens
	u.Output += e.OutputTokens
	u.CacheRead += e.CacheReadTokens
	u.CacheWrite += e.CacheCreationTokens
	into[e.Model] = u
}

func newestTicket(events []Event, in func(int) bool) string {
	k := ""
	for i, e := range events {
		if e.TicketKey != "" && in(i) {
			k = e.TicketKey
		}
	}
	return k
}

// byInstant is a copy of events in time order. Hook lines write +00:00 and extracted lines Z
// with fractions, so their text does not sort. Equal instants keep the order given, which is
// the ledger's order of writing.
func byInstant(events []Event) []Event {
	// Each time is parsed once and indexes are sorted, not the events: an Event is large, and a
	// PR's sessions can hold a hundred thousand of them.
	at := make([]time.Time, len(events))
	order := make([]int, len(events))
	for i := range events {
		at[i], _ = parseTS(events[i].TS)
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int {
		if c := at[a].Compare(at[b]); c != 0 {
			return c
		}
		return a - b
	})
	out := make([]Event, len(events))
	for i, j := range order {
		out[i] = events[j]
	}
	return out
}

func parseTS(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func short(sha string) string {
	if len(sha) > shortSHA {
		return sha[:shortSHA]
	}
	return sha
}
