package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/0xbew/livy/internal/extract"
	"github.com/0xbew/livy/internal/pytext"
)

// syntheticModel is the model Claude Code writes its own zero-token messages under: not a call.
const syntheticModel = "<synthetic>"

// Harness texts, read from real transcripts. Matched and discarded, never kept.
const (
	rejectionMarker     = "The user doesn't want to proceed with this tool use"
	toolInterruptMarker = "The user doesn't want to take this action"
	interruptMarker     = "[Request interrupted by user"
	bashRanErrorPrefix  = "Exit code "
	toolUseError        = "<tool_use_error>"
	outcomeCarryCap     = 256
)

// knownTypes are the record types this reader has seen. Any other is format drift: the file
// is held before it and a ledger.error names it.
var knownTypes = map[string]bool{}

func init() {
	for _, t := range []string{"assistant", "attachment", "user", "api-request", "api-request-blob",
		"queue-operation", "last-prompt", "atis-latch", "ai-title", "pr-link", "mode", "system",
		"bridge-session", "file-history-snapshot", "frame-link", "api-request-shape", "cost-state",
		"custom-title", "agent-name", "artifact-autoreact-ledger", "file-history-delta", "tools-used",
		"progress", "permission-mode", "artifact-comment-monitor", "relocated", "started", "result",
		"session-owner", "fork-context-ref", "summary"} {
		knownTypes[t] = true
	}
}

var (
	harnessBlockPrefixes   = []string{"<ide_", "<system-reminder"}
	harnessMessagePrefixes = []string{"<task-notification", "<local-command"}
	planTools              = map[string]string{"EnterPlanMode": "enter", "ExitPlanMode": "exit"}
	toolIDRe               = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	idHashRe               = regexp.MustCompile(`^[0-9a-f]{16}$`)
	promptLinkRe           = regexp.MustCompile(`^prompt:[A-Za-z0-9-]{1,64}$`)
	classifierReason       = regexp.MustCompile(`^Permission for this action was denied by the Claude Code auto mode classifier\. ` +
		`Reason: \[([A-Za-z][A-Za-z0-9 /&+.'-]{0,62})\]`)
	parserReason     = regexp.MustCompile(`^Contains ([a-z][a-z0-9_]{2,62})$`)
	bashApprovalRule = regexp.MustCompile(`^(?:Contains [a-z]|Brace expansion \(|'export' operand )`)
	specialTools     = []string{"Bash"}
	outcomeRanTools  = map[string]bool{"Bash": true}
	blockRules       []blockRule
)

type blockRule struct {
	re                *regexp.Regexp
	outcome, category string
}

func init() {
	q := regexp.QuoteMeta
	for _, r := range [][3]string{
		{`Permission for this action was denied by the Claude Code auto mode classifier\b`, "blocked", "classifier_denied"},
		{`The server-side auto mode classifier gave no verdict\b`, "blocked", "classifier_unavailable"},
		{`\S+ is temporarily unavailable \([^)\n]{1,80}\), so auto mode cannot determine`, "blocked", "classifier_unavailable"},
		{`Auto mode could not evaluate this action and is blocking it`, "blocked", "safety_blocked"},
		{`The user doesn't want to proceed with this tool use`, "blocked", "user_rejected"},
		{`Permission to use \S+ has been denied`, "blocked", "permission_denied"},
		{`(?:This command requires approval|This \S+ command contains multiple operations\. ` +
			`The following part requires approval|\S+ command requires approval|Claude requested permissions to )`,
			"blocked", "approval_required"},
		{`This agent is isolated in the worktree `, "blocked", "worktree_isolation"},
		{q(toolUseError) + `Blocked: `, "blocked", "harness_rule"},
		{`The user doesn't want to take this action`, "not_run", "user_interrupted"},
		{q(toolUseError) + `Cancelled: `, "not_run", "cancelled"},
		{q(toolUseError) + `InputValidationError`, "not_run", "invalid_input"},
		{`Tool permission request failed`, "not_run", "permission_request_failed"},
	} {
		blockRules = append(blockRules, blockRule{regexp.MustCompile(`^` + r[0]), r[1], r[2]})
	}
}

// ctxT is where the human is: whether any folder is known yet, its repo ("" out of scope)
// and branch.
type ctxT struct {
	Known  bool
	Repo   string
	Branch string
}

// kept is the record of one API call kept for usage: the fields its line is built from.
type kept struct {
	output int64
	rec    map[string]any // slimmed: no content
	named  []*extract.Named
}

type interaction struct {
	Kind, ID       string
	Scopable       bool
	Repo, Branch   string
	Timestamp      any
	SessionID      any
	Version        any
	PromptChars    *int
	PermissionMode string
	AfterError     *bool
	SlashCommand   *bool
	PlanModeAction string
}

type detail struct {
	MessageID  string
	Tools      []string
	toolIDs    map[string]bool
	Named      []*extract.Named
	BadFields  map[string]bool
	BadTools   int
	Prompt     string
	Fields     map[string]any // sessionId, gitBranch, cwd, timestamp, agentId, isSidechain as last seen
	Synthetic  bool
	Incomplete bool
	StopReason string
	Error      *string
	Extra      map[string]*string // attributionAgent, attributionSkill, perTurnEffort: first seen
	extraOrder []string
}

type decided struct {
	outcome, category, reason string
	none                      bool
}

type outcomeItem struct {
	index     int
	where     map[string]any // sessionId, gitBranch, cwd, timestamp, version, isSidechain, agentId
	skip      string
	toolUseID string
	asSpecial map[string]decided
	asOther   decided
	// filled at finish
	tool        string
	result      decided
	unnamed     bool
	carriedRepo string
	named       *extract.Named
}

type callWhere struct {
	fields map[string]any // cwd, gitBranch when present
	named  *extract.Named
}

// outcomeCarry is what the next pass needs about calls whose result is after the cut.
type outcomeCarry struct {
	Pending   map[string]string    `json:"pending"`
	Started   map[string][]any     `json:"started"`
	Elsewhere []string             `json:"elsewhere"`
	order     []string             // pending ids, newest first, for a stable cap
	where     map[string][2]string // carried: id -> repo, branch ("" = none)
}

// drift is the first record this reader does not recognise: the file is held before it.
type drift struct {
	index   int
	version string
	what    string
}

type parsed struct {
	messages        []kept
	prLinks         []map[string]any
	interactions    []interaction
	details         []*detail
	outcomes        []*outcomeItem
	nextOffset      int64
	assistant       int
	usable          int
	synthetic       int
	badLines        int
	readError       bool
	carry           map[string]any
	detailCarry     map[string]any
	outcomeCarry    map[string]any
	unrecognisedUsr int
	drift           *drift
}

func hashHex(s string, n int) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:n]
}

func idHash(toolID string) string { return hashHex(toolID, 16) }

func isSlug(v string) bool {
	parts := strings.Split(v, "/")
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if p == "" || strings.HasPrefix(p, ".") || pytext.Strip(p) != p {
			return false
		}
	}
	return true
}

func textOf(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, b := range c {
			if m, ok := b.(map[string]any); ok && m["type"] == "text" {
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func humanText(content any) string {
	var blocks []string
	switch c := content.(type) {
	case string:
		blocks = []string{c}
	case []any:
		for _, b := range c {
			if m, ok := b.(map[string]any); ok && m["type"] == "text" {
				if t, ok := m["text"].(string); ok {
					blocks = append(blocks, t)
				}
			}
		}
	default:
		return ""
	}
	var keep []string
	for _, b := range blocks {
		lead := strings.TrimLeftFunc(b, pytext.IsSpace)
		harness := false
		for _, p := range harnessBlockPrefixes {
			harness = harness || strings.HasPrefix(lead, p)
		}
		if !harness {
			keep = append(keep, b)
		}
	}
	return strings.Join(keep, " ")
}

// containsString is true when s appears in any string (or key) inside v.
func containsString(v any, s string) bool {
	switch t := v.(type) {
	case string:
		return strings.Contains(t, s)
	case []any:
		for _, x := range t {
			if containsString(x, s) {
				return true
			}
		}
	case map[string]any:
		for k, x := range t {
			if strings.Contains(k, s) || containsString(x, s) {
				return true
			}
		}
	}
	return false
}

func classify(result map[string]any, tool string) decided {
	ran := outcomeRanTools[tool]
	if result["is_error"] != true {
		if ran {
			return decided{outcome: "ran"}
		}
		return decided{none: true}
	}
	var text string
	switch c := result["content"].(type) {
	case string:
		text = c
	case []any:
		text = textOf(c)
	default:
		if ran {
			return decided{outcome: "unknown", category: "unrecognised"}
		}
		return decided{none: true}
	}
	text = strings.TrimLeftFunc(text, pytext.IsSpace)
	if d, ok := byBlockRule(text, ran); ok {
		return d
	}
	if tool == "Bash" {
		if d, ok := bashDecision(text); ok {
			return d
		}
	}
	if ran {
		return decided{outcome: "unknown", category: "unrecognised"}
	}
	return decided{none: true}
}

// byBlockRule is the decision of the first block rule the error text matches; false when
// none does.
func byBlockRule(text string, ran bool) (decided, bool) {
	for _, r := range blockRules {
		if !r.re.MatchString(text) {
			continue
		}
		if r.outcome != "blocked" && !ran {
			return decided{none: true}, true
		}
		reason := ""
		if r.category == "classifier_denied" {
			if m := classifierReason.FindStringSubmatch(text); m != nil {
				reason = m[1]
			}
		}
		return decided{outcome: r.outcome, category: r.category, reason: reason}, true
	}
	return decided{}, false
}

// bashDecision is what a Bash error text says happened: it ran and failed, needed approval,
// or was refused as a tool-use error; false when it says none of these.
func bashDecision(text string) (decided, bool) {
	if strings.HasPrefix(text, bashRanErrorPrefix) {
		return decided{outcome: "ran"}, true
	}
	if bashApprovalRule.MatchString(text) {
		reason := ""
		if m := parserReason.FindStringSubmatch(pytext.Strip(text)); m != nil {
			reason = m[1]
		}
		return decided{outcome: "blocked", category: "approval_required", reason: reason}, true
	}
	if strings.HasPrefix(text, toolUseError) {
		return decided{outcome: "not_run", category: "tool_use_error"}, true
	}
	return decided{}, false
}

// reader is one pass over one transcript.
type reader struct {
	scope *extract.Scope
}

func (rd *reader) contextFrom(rec map[string]any) *ctxT {
	cwd, ok := rec["cwd"].(string)
	if !ok {
		return nil
	}
	repo := rd.scope.RepoFor(cwd)
	branch := ""
	if b, ok := extract.Str(rec["gitBranch"]); ok && repo != "" && repo != extract.Unknown {
		branch = b
	}
	return &ctxT{true, repo, branch}
}

// interactionsOf is the interaction facts in one record, the updated after-error flag, and
// whether the record is a user message of a shape this reader does not know.
func (rd *reader) interactionsOf(rec map[string]any, ctx ctxT, afterError bool, lineOffset int64) ([]interaction, bool, bool) {
	if rec["isSidechain"] == true {
		return nil, afterError, false
	}
	where := ctx
	if own := rd.contextFrom(rec); own != nil {
		where = *own
	}
	r := recordFacts{rec: rec, where: where, lineOffset: lineOffset}
	switch rec["type"] {
	case "queue-operation":
		return r.midTurn(), afterError, false
	case "assistant":
		return r.agentTools(), afterError, false
	case "user":
		return r.userFacts(afterError)
	}
	return nil, afterError, false
}

// recordFacts builds the interactions of one record, placed where it worked.
type recordFacts struct {
	rec        map[string]any
	where      ctxT
	lineOffset int64
}

// fallback is the id of an interaction whose record carries none of its own.
func (r recordFacts) fallback(what string, position int) string {
	seed := extract.PyStr(r.rec["sessionId"]) + "|" + extract.PyStr(r.rec["timestamp"]) + "|" + what + "|" +
		itoa(r.lineOffset) + "|" + itoa(int64(position))
	return hashHex(seed, 24)
}

// slim is an interaction of this record with only its kind, id and placing.
func (r recordFacts) slim(what, base string) interaction {
	return interaction{Kind: what, ID: what + ":" + base, Scopable: r.where.Known, Repo: r.where.Repo,
		Branch: r.where.Branch, Timestamp: r.rec["timestamp"], SessionID: r.rec["sessionId"], Version: r.rec["version"]}
}

// uuidOr is the record's uuid, or a fallback id when it has none.
func (r recordFacts) uuidOr(what string) string {
	if uid, _ := r.rec["uuid"].(string); uid != "" {
		return uid
	}
	return r.fallback(what, 0)
}

// midTurn is a prompt absorbed mid-turn, from a queue operation.
func (r recordFacts) midTurn() []interaction {
	if r.rec["operation"] == "remove" && r.rec["reason"] == "absorbed_mid_turn" {
		return []interaction{r.slim("midturn", r.fallback("midturn", 0))}
	}
	return nil
}

// agentTools is the questions the agent asked and its plan-mode moves, from its tool calls.
func (r recordFacts) agentTools() []interaction {
	var found []interaction
	msg, _ := r.rec["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	for position, b := range content {
		block, ok := b.(map[string]any)
		if !ok || block["type"] != "tool_use" {
			continue
		}
		name, ok := block["name"].(string)
		if !ok {
			continue
		}
		toolID, ok := block["id"].(string)
		if !ok {
			toolID = r.fallback(name, position)
		}
		if name == "AskUserQuestion" {
			found = append(found, r.slim("agent_asked", toolID))
		} else if action, ok := planTools[name]; ok {
			it := r.slim("plan_mode", toolID)
			it.PlanModeAction = action
			found = append(found, it)
		}
	}
	return found
}

// userFacts is a user record's interactions, the updated after-error flag, and whether its
// message has a shape this reader does not know.
func (r recordFacts) userFacts(afterError bool) ([]interaction, bool, bool) {
	rec := r.rec
	if pytext.Truthy(rec["isCompactSummary"]) {
		return []interaction{r.slim("compacted", r.uuidOr("compacted"))}, afterError, false
	}
	if pytext.Truthy(rec["isMeta"]) || pytext.Truthy(rec["isVisibleInTranscriptOnly"]) {
		return nil, afterError, false
	}
	msg, ok := rec["message"].(map[string]any)
	if !ok {
		return nil, afterError, false
	}
	content := msg["content"]
	list, isList := content.([]any)
	if _, isStr := content.(string); !isStr && !isList {
		return nil, afterError, true
	}
	results := toolResults(list)
	var found []interaction
	interrupted := strings.Contains(textOf(content), interruptMarker)
	if interrupted {
		found = append(found, r.slim("interrupt", r.uuidOr("interrupt")))
	}
	failed, failedHere := r.toolErrors(results)
	found = append(found, failed...)
	if len(results) > 0 {
		afterError = failedHere
	}
	if len(results) > 0 || interrupted {
		return found, afterError, false
	}
	it, ok := r.prompt(content, afterError)
	if !ok {
		return found, afterError, false
	}
	return append(found, it), false, false
}

// toolResults is the tool_result blocks in a user message's content.
func toolResults(list []any) []map[string]any {
	var results []map[string]any
	for _, b := range list {
		if m, ok := b.(map[string]any); ok && m["type"] == "tool_result" {
			results = append(results, m)
		}
	}
	return results
}

// toolErrors is the rejected and failed tool calls among results, and whether any failed
// (a rejection or an Esc mid-tool is not a failure).
func (r recordFacts) toolErrors(results []map[string]any) ([]interaction, bool) {
	var found []interaction
	failedHere := false
	for position, result := range results {
		if result["is_error"] != true {
			continue
		}
		failedID, _ := result["tool_use_id"].(string)
		pick := func(what string) string {
			if failedID != "" {
				return failedID
			}
			return r.fallback(what, position)
		}
		body := result["content"]
		if containsString(body, rejectionMarker) {
			found = append(found, r.slim("rejected", pick("rejected")))
		} else if containsString(body, toolInterruptMarker) {
			// Esc mid-tool: the interrupt marker records it.
		} else {
			found = append(found, r.slim("tool_error", pick("tool_error")))
			failedHere = true
		}
	}
	return found, failedHere
}

// prompt is the human prompt in a user message's content, or false when it holds none.
func (r recordFacts) prompt(content any, afterError bool) (interaction, bool) {
	human := pytext.Strip(humanText(content))
	if human == "" {
		return interaction{}, false
	}
	for _, p := range harnessMessagePrefixes {
		if strings.HasPrefix(human, p) {
			return interaction{}, false
		}
	}
	it := r.slim("prompt", r.uuidOr("prompt"))
	chars := utf8.RuneCountInString(human)
	it.PromptChars = &chars
	if m, ok := extract.Str(r.rec["permissionMode"]); ok {
		it.PermissionMode = m
	}
	ae := afterError
	it.AfterError = &ae
	slash := strings.HasPrefix(human, "<command-")
	it.SlashCommand = &slash
	return it, true
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

// userBlocks are the content block types a user message carries. Another type may be what
// a prompt or a tool result now looks like, so it is format drift, not something to skip.
var userBlocks = map[string]bool{"text": true, "tool_result": true, "image": true, "document": true}

// unknownUserShape is the drift a user record of an unknown shape is reported as.
const unknownUserShape = "a user message of a shape this extractor does not recognise"

// recordVersion is the Claude Code version a record names, for an error line: a name, never
// free text.
func recordVersion(rec map[string]any) string {
	if v, ok := rec["version"].(string); ok && extract.IsName(v) {
		return v
	}
	return "unknown"
}

// userShapeKnown is true for a user record whose message this reader knows how to read.
func userShapeKnown(rec map[string]any) bool {
	msg, ok := rec["message"].(map[string]any)
	if !ok {
		return false
	}
	switch c := msg["content"].(type) {
	case string:
		return true
	case []any:
		for _, b := range c {
			block, ok := b.(map[string]any)
			if !ok {
				return false
			}
			if t, _ := block["type"].(string); !userBlocks[t] {
				return false
			}
		}
		return true
	}
	return false
}

func usable(rec map[string]any) bool {
	msg, ok := rec["message"].(map[string]any)
	if !ok {
		return false
	}
	if _, ok := msg["id"].(string); !ok {
		return false
	}
	usage, ok := msg["usage"].(map[string]any)
	if !ok {
		return false
	}
	_, a := extract.Int(usage["input_tokens"])
	_, b := extract.Int(usage["output_tokens"])
	return a && b
}

var slimKeys = []string{"sessionId", "requestId", "version", "agentId", "effort", "isSidechain", "gitBranch", "cwd", "timestamp"}

func slimRecord(rec map[string]any) map[string]any {
	keep := map[string]any{}
	for _, k := range slimKeys {
		if v, ok := rec[k]; ok {
			keep[k] = v
		}
	}
	msg, _ := rec["message"].(map[string]any)
	m := map[string]any{}
	for _, k := range []string{"id", "model", "usage"} {
		if v, ok := msg[k]; ok {
			m[k] = v
		}
	}
	keep["message"] = m
	return keep
}

// cut is the index before which every message is complete, for a file that may still grow:
// the last message to begin, widened to every message reaching at or past the cut.
func cut(spans map[string][2]int, end int) int {
	if len(spans) == 0 {
		return end
	}
	c := -1
	for _, s := range spans {
		if s[0] > c {
			c = s[0]
		}
	}
	for {
		widened := c
		first := true
		for _, s := range spans {
			if s[1] >= c && (first || s[0] < widened) {
				widened, first = s[0], false
			}
		}
		if widened == c {
			return c
		}
		c = widened
	}
}

func cwdOf(m map[string]any) (string, bool) {
	s, ok := m["cwd"].(string)
	return s, ok
}

// readTranscript parses one transcript from the smallest of four family offsets to its last
// complete line, or to the first record it does not recognise. Each family takes only lines
// at or past its own offset; context (where the human is, whether the last call failed, the
// prompt in effect, which tool each pending call was) is tracked across every line read.
func (rd *reader) readTranscript(path string, start int64, settled bool, interactionStart int64, carry any,
	detailStart int64, detailCarry any, outcomeStart int64, outcomeCarryIn any) *parsed {
	s := rd.newScan(start, interactionStart, detailStart, outcomeStart)
	s.loadCarries(carry, detailCarry, outcomeCarryIn)
	if err := s.readLines(path); err != nil {
		s.res.readError = true
		return s.res
	}
	s.finish(settled)
	return s.res
}

func stringsAny(s []string) []any {
	out := make([]any, 0, len(s))
	for _, v := range s {
		out = append(out, v)
	}
	return out
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (rd *reader) orgs() func() []string {
	return rd.scope.Orgs
}

// carried is the tool names, start places and out-of-scope hashes the last pass left, in
// exactly the shape this reader writes: they come from a state file and go into lines.
func (rd *reader) carried(carry any) (map[string]string, map[string][2]string, map[string]bool) {
	names, where, elsewhere := map[string]string{}, map[string][2]string{}, map[string]bool{}
	c, ok := carry.(map[string]any)
	if !ok {
		return names, where, elsewhere
	}
	pending, ok := c["pending"].(map[string]any)
	if !ok {
		return names, where, elsewhere
	}
	carriedElsewhere(c["elsewhere"], elsewhere)
	carriedNames(pending, names)
	started, _ := c["started"].(map[string]any)
	rd.carriedWhere(started, names, where)
	return names, where, elsewhere
}

// carriedElsewhere adds the hashed ids of calls whose outcome landed in another file.
func carriedElsewhere(v any, elsewhere map[string]bool) {
	hashed, ok := v.([]any)
	if !ok {
		return
	}
	for i, h := range hashed {
		if i >= outcomeCarryCap {
			break
		}
		if s, ok := h.(string); ok && idHashRe.MatchString(s) {
			elsewhere[s] = true
		}
	}
}

// carriedNames adds the pending calls' tool names. A JSON object's order is lost here; the
// cap applies to at most 256 written entries, so a state file this reader wrote never
// exceeds it.
func carriedNames(pending map[string]any, names map[string]string) {
	n := 0
	for k, v := range pending {
		if n >= outcomeCarryCap {
			break
		}
		n++
		if s, ok := v.(string); ok && toolIDRe.MatchString(k) && extract.IsName(s) {
			names[k] = s
		}
	}
}

// carriedWhere adds where each pending call started, when that repo is still in scope.
func (rd *reader) carriedWhere(started map[string]any, names map[string]string, where map[string][2]string) {
	for id, v := range started {
		if _, ok := names[id]; !ok {
			continue
		}
		pair, ok := v.([]any)
		if !ok || len(pair) != 2 {
			continue
		}
		repo, ok := pair[0].(string)
		if !ok || !isSlug(repo) || !rd.scope.Git.InScope("https://github.com/"+repo, rd.scope.Orgs) {
			continue
		}
		switch b := pair[1].(type) {
		case nil:
			where[id] = [2]string{repo, ""}
		case string:
			where[id] = [2]string{repo, b}
		}
	}
}

func (rd *reader) finishOutcome(it *outcomeItem, nameOf func(string) (string, bool), callAt map[string]callWhere,
	carriedWhere map[string][2]string, elsewhere map[string]bool) *outcomeItem {
	if it.asSpecial == nil {
		return it // a marker: a result with no usable id
	}
	tool, named := nameOf(it.toolUseID)
	if !named && elsewhere[idHash(it.toolUseID)] {
		return nil // a call from an earlier pass that started out of scope: no trace
	}
	if cw, ok := callAt[it.toolUseID]; ok {
		w := map[string]any{}
		for k, v := range it.where {
			w[k] = v
		}
		w["gitBranch"] = nil
		for k, v := range cw.fields {
			w[k] = v
		}
		it.where = w
		it.named = cw.named
	} else if cr, ok := carriedWhere[it.toolUseID]; ok {
		w := map[string]any{}
		for k, v := range it.where {
			w[k] = v
		}
		w["gitBranch"] = nilIfEmpty(cr[1])
		it.where = w
		it.carriedRepo = cr[0]
	}
	d := it.asOther
	if sd, ok := it.asSpecial[tool]; ok && named {
		d = sd
	}
	if d.none {
		if !named {
			it.skip = "unnamed"
			return it
		}
		return nil
	}
	it.tool, it.result, it.unnamed = tool, d, !named
	return it
}

func (rd *reader) collectDetail(rec map[string]any, details map[string]*detail, order *[]string, prompt string, lineOffset int64) {
	msg, ok := rec["message"].(map[string]any)
	if !ok {
		return
	}
	id, ok := msg["id"].(string)
	if !ok {
		return
	}
	d := details[id]
	if d == nil {
		d = newDetail(id, prompt, rec["isSidechain"] == true)
		details[id] = d
		*order = append(*order, id)
	}
	for _, k := range []string{"sessionId", "gitBranch", "cwd", "timestamp", "agentId", "isSidechain"} {
		if v, ok := rec[k]; ok {
			d.Fields[k] = v
		}
	}
	if msg["model"] == syntheticModel {
		d.Synthetic = true
	}
	content, ok := msg["content"].([]any)
	if !ok {
		d.Incomplete = true
		content = nil
	}
	cwd, _ := rec["cwd"].(string)
	d.addTools(content, cwd, lineOffset)
	if stop := nameField(msg["stop_reason"], d, "stop_reason"); stop != "" {
		d.StopReason = stop
	}
	if rec["isApiErrorMessage"] == true {
		e := nameField(rec["error"], d, "error")
		if e == "" {
			e = "unknown"
		}
		d.Error = &e
	}
	for _, k := range []string{"attributionAgent", "attributionSkill", "perTurnEffort"} {
		if _, seen := d.Extra[k]; !seen && rec[k] != nil {
			v := nameField(rec[k], d, k)
			d.Extra[k] = &v
			d.extraOrder = append(d.extraOrder, k)
		}
	}
}

// newDetail starts a call's detail; a subagent's call carries no prompt.
func newDetail(id, prompt string, sidechain bool) *detail {
	p := prompt
	if sidechain {
		p = ""
	}
	return &detail{MessageID: id, toolIDs: map[string]bool{}, BadFields: map[string]bool{}, Prompt: p,
		Fields: map[string]any{}, Extra: map[string]*string{}}
}

// addTools adds each tool call in content not seen before, with where it names; a call with
// no usable name marks the detail incomplete.
func (d *detail) addTools(content []any, cwd string, lineOffset int64) {
	for position, b := range content {
		block, ok := b.(map[string]any)
		if !ok || block["type"] != "tool_use" {
			continue
		}
		toolID, ok := block["id"].(string)
		if !ok {
			toolID = itoa(lineOffset) + ":" + itoa(int64(position))
		}
		if d.toolIDs[toolID] {
			continue
		}
		d.toolIDs[toolID] = true
		name, ok := block["name"].(string)
		if !ok || !extract.IsName(name) {
			d.BadTools++
			d.Incomplete = true
			continue
		}
		d.Tools = append(d.Tools, name)
		d.Named = append(d.Named, extract.NamedBy(name, block["input"], cwd))
	}
}

func nameField(v any, d *detail, field string) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok && extract.IsName(s) {
		return s
	}
	d.BadFields[field] = true
	return ""
}

// keep holds each call's record with the highest output count, with every folder its
// records named.
func (rd *reader) keep(best map[string]*kept, order *[]string, rec map[string]any) {
	msg := rec["message"].(map[string]any)
	usage := msg["usage"].(map[string]any)
	output, _ := extract.Int(usage["output_tokens"])
	id := msg["id"].(string)
	cwd, _ := rec["cwd"].(string)
	var named []*extract.Named
	if content, ok := msg["content"].([]any); ok {
		for _, b := range content {
			block, ok := b.(map[string]any)
			if !ok || block["type"] != "tool_use" {
				continue
			}
			name, _ := block["name"].(string)
			if n := extract.NamedBy(name, block["input"], cwd); n != nil {
				named = append(named, n)
			}
		}
	}
	k := best[id]
	if k != nil {
		named = append(append([]*extract.Named{}, k.named...), named...)
	}
	if k == nil || output >= k.output {
		if k == nil {
			*order = append(*order, id)
		}
		best[id] = &kept{output, slimRecord(rec), named}
	} else {
		k.named = named
	}
}
