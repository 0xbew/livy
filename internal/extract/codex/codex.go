// Package codex extracts per-call token usage from Codex session rollouts
// ($CODEX_HOME/sessions/**/rollout-*.jsonl, CODEX_HOME defaulting to ~/.codex) into the
// ledger as usage.api_call, one line per model response.
//
// Codex writes one token_usage_record per response, once, after it completes: its
// response_id is the call's id, its usage the call's tokens. The turn's model, effort and
// working folder come from the turn_context before it. Tokens are written in Claude Code's
// terms, as the OTEL path already does for Codex: input_tokens is the uncached input
// (Codex's input minus cached reads and cache writes), cache_read_input_tokens the cached
// input, cache_creation_input_tokens the cache writes, and thinking_tokens the reasoning
// tokens (already inside output_tokens, as Claude's thinking tokens are).
//
// Facts only: ids, counts, the model and effort names, the repo slug. No message text, no
// folder. A file is read in full each pass for its turn contexts (rollouts are small) and
// emits only the records past its stored offset, so a re-run never writes a call twice.
// A record type or usage shape this reader does not know holds the file before it and
// writes a ledger.error naming the Codex version, once per held position. Codex's own
// running total (event_msg token_count) is checked against the sum of the calls read: a
// difference is said, never silently absorbed.
package codex

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/0xbew/livy/internal/extract"
)

const stateName = "extract-codex-state.json"

// DataNames are the files and folders this package keeps in the data folder, by exact name.
// `livy uninstall --purge` deletes these and nothing else.
func DataNames() []string { return []string{stateName} }

// Record types seen in Codex rollouts. Any other is format drift.
var knownTypes = map[string]bool{"session_meta": true, "turn_context": true, "response_item": true,
	"event_msg": true, "token_usage_record": true, "world_state": true, "compacted": true}

var tokenFields = []string{"input_tokens", "cached_input_tokens", "cache_write_input_tokens", "output_tokens",
	"reasoning_output_tokens", "total_tokens"}

// Root is $CODEX_HOME/sessions, else ~/.codex/sessions.
func Root() string {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		h, _ := os.UserHomeDir()
		home = filepath.Join(h, ".codex")
	}
	return filepath.Join(home, "sessions")
}

// Stats are the pass's counts.
type Stats map[string]int

type turn struct {
	model, effort, cwd string
	haveCwd            bool
}

type usage struct{ v [6]int64 }

func readUsage(v any) (usage, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return usage{}, false
	}
	var u usage
	for i, f := range tokenFields {
		n, ok := extract.Int(m[f])
		if !ok {
			return usage{}, false
		}
		u.v[i] = n
	}
	return u, true
}

type held struct{ version, what string }

type fileResult struct {
	lines   []map[string]any
	next    int64
	drift   *held
	sumOK   bool // every token_count total seen matched the calls before it
	checked int  // token_count totals compared
	outside int  // calls out of scope
	noTs    int
	noTurn  int
	calls   int
	ids     []string // response ids written from this file, to forget when it is held
}

func hashHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:16]
}

// files lists every rollout under root, sorted.
func files(root string, unreadable *int) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if p != root || !os.IsNotExist(err) {
				*unreadable++
			}
			return nil
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), ".jsonl") {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func read(path string, start int64, scope *extract.Scope, seen map[string]bool) (*fileResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ro := &rolloutRead{start: start, scope: scope, seen: seen, res: &fileResult{next: start, sumOK: true},
		turns: map[string]turn{}, version: "unknown"}
	br := bufio.NewReaderSize(f, 1<<20)
	var offset int64
	for {
		raw, err := br.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		if len(raw) == 0 || raw[len(raw)-1] != '\n' {
			break
		}
		lineOffset := offset
		offset += int64(len(raw))
		if ro.line(raw, lineOffset) {
			return ro.res, nil
		}
	}
	ro.res.next = max(start, offset)
	return ro.res, nil
}

// rolloutRead is one read of a rollout file: the context carried across its lines (the Codex
// version, the session, each turn's model and folder, the running token total).
type rolloutRead struct {
	start   int64
	scope   *extract.Scope
	seen    map[string]bool
	res     *fileResult
	turns   map[string]turn
	running usage
	version string
	session string
}

// line takes one complete line; true when the file is held before it.
func (ro *rolloutRead) line(raw []byte, lineOffset int64) bool {
	emit := lineOffset >= ro.start
	v, derr := extract.Decode(raw)
	rec, ok := v.(map[string]any)
	if derr != nil || !ok {
		return emit && ro.hold("a line that is not a JSON object", lineOffset)
	}
	kind, _ := rec["type"].(string)
	payload, _ := rec["payload"].(map[string]any)
	if !knownTypes[kind] {
		what := "a record of an unknown type"
		if extract.IsName(kind) {
			what = "a record of the unknown type " + kind
		}
		return emit && ro.hold(what, lineOffset)
	}
	problem := ""
	switch kind {
	case "session_meta":
		ro.sessionMeta(payload)
	case "turn_context":
		ro.turnContext(payload)
	case "event_msg":
		problem = ro.tokenCount(payload, emit)
	case "token_usage_record":
		problem = ro.usageRecord(rec, payload, emit)
	}
	return problem != "" && emit && ro.hold(problem, lineOffset)
}

// hold records the drift and holds the file before the line at lineOffset.
func (ro *rolloutRead) hold(what string, lineOffset int64) bool {
	ro.res.drift = &held{ro.version, what}
	ro.res.next = lineOffset
	return true
}

func (ro *rolloutRead) sessionMeta(payload map[string]any) {
	if s, ok := extract.Str(payload["cli_version"]); ok && extract.IsName(s) {
		ro.version = s
	}
	if s, ok := extract.Str(payload["id"]); ok && extract.IsName(s) {
		ro.session = s
	}
}

func (ro *rolloutRead) turnContext(payload map[string]any) {
	id, ok := extract.Str(payload["turn_id"])
	if !ok || !extract.IsName(id) {
		return
	}
	t := turn{}
	if s, ok := extract.Str(payload["model"]); ok && extract.IsName(s) {
		t.model = s
	}
	if s, ok := extract.Str(payload["effort"]); ok && extract.IsName(s) {
		t.effort = s
	}
	t.cwd, t.haveCwd = payload["cwd"].(string)
	ro.turns[id] = t
}

// tokenCount checks a token_count's total against the calls before it; it returns what is
// not recognised about it, or "".
func (ro *rolloutRead) tokenCount(payload map[string]any, emit bool) string {
	if payload["type"] != "token_count" {
		return ""
	}
	if payload["info"] == nil {
		return "" // a rate-limit-only update carries no totals
	}
	info, isMap := payload["info"].(map[string]any)
	if !isMap {
		return "a token_count whose totals this reader does not recognise"
	}
	total, ok := readUsage(info["total_token_usage"])
	if !ok {
		return "a token_count whose totals this reader does not recognise"
	}
	if emit {
		ro.res.checked++
		ro.res.sumOK = ro.res.sumOK && total == ro.running
	}
	return ""
}

// usageRecord adds one call to the running total and, past the offset, writes it once; it
// returns what is not recognised about it, or "".
func (ro *rolloutRead) usageRecord(rec, payload map[string]any, emit bool) string {
	id, ok1 := extract.Str(payload["response_id"])
	ok1 = ok1 && extract.IsName(id)
	u, ok2 := readUsage(payload["usage"])
	if !ok1 || !ok2 {
		return "a token_usage_record whose shape this reader does not recognise"
	}
	for i := range ro.running.v {
		ro.running.v[i] += u.v[i]
	}
	if !emit || ro.seen[id] {
		return ""
	}
	ro.res.calls++
	// Marked seen only once written: a copy out of scope must not hide an in-scope one.
	if line := callLine(rec, payload, id, u, ro.version, ro.session, ro.turns, ro.scope, ro.res); line != nil {
		ro.res.lines = append(ro.res.lines, line)
		ro.res.ids = append(ro.res.ids, id)
		ro.seen[id] = true
	}
	return ""
}

func callLine(rec, payload map[string]any, id string, u usage, version, session string, turns map[string]turn,
	scope *extract.Scope, res *fileResult) map[string]any {
	turnID, _ := extract.Str(payload["turn_id"])
	if !extract.IsName(turnID) {
		turnID = ""
	}
	t, ok := turns[turnID]
	if !ok || !t.haveCwd {
		res.noTurn++
		return nil
	}
	repo := scope.RepoFor(t.cwd)
	if repo == "" {
		res.outside++
		return nil
	}
	when := extract.UTCInstant(rec["timestamp"])
	if when == nil {
		res.noTs++
		return nil
	}
	branch := extract.Unknown
	if repo != extract.Unknown {
		if _, b, ok := scope.BranchAt(t.cwd, when); ok {
			branch = b
		}
	}
	in, cached, write, out, reasoning := u.v[0], u.v[1], u.v[2], u.v[3], u.v[4]
	line := map[string]any{"schema_version": int64(extract.SchemaVersion), "ts": extract.UTCSeconds(rec["timestamp"]),
		"event": "usage.api_call", "repo": repo, "branch": branch, "source": "transcript", "harness": "codex",
		"harness_version": version, "message_id": id, "output_tokens": out, "cache_read_input_tokens": cached,
		"cache_creation_input_tokens": write, "thinking_tokens": reasoning}
	// Codex counts cached reads and cache writes inside its input; Claude Code does not.
	if cached+write <= in {
		line["input_tokens"] = in - cached - write
	}
	if s, ok := extract.Str(payload["session_id"]); ok && extract.IsName(s) {
		line["session_id"] = s
	} else if session != "" {
		line["session_id"] = session
	}
	if turnID != "" {
		line["turn_id"] = turnID
	}
	if t.model != "" {
		line["model"] = t.model
	}
	if t.effort != "" {
		line["effort"] = t.effort
	}
	return line
}

// Run is one incremental pass over every rollout under root, writing through w.
func Run(root, dir string, w *extract.Writer, stderr io.Writer) Stats {
	stats := Stats{}
	statePath := filepath.Join(dir, stateName)
	state, ok := openState(statePath, w, stats)
	if !ok {
		return stats
	}
	offsets := extract.SubMap(state, "files")
	heldAt := extract.SubMap(state, "held")
	written, err := extract.LoadWritten(dir)
	if err != nil {
		w.Error(fmt.Sprintf("extract: the log of lines already written could not be read (%T); a Codex pass that was cut short may append some lines again", err))
	}
	cp := &codexPass{stats: stats, w: w, written: written, scope: extract.NewScope(stderr), seen: map[string]bool{},
		offsets: offsets, heldAt: heldAt}
	unreadable := 0
	for _, path := range files(root, &unreadable) {
		if !cp.file(path) {
			break
		}
	}
	problems, mismatched := cp.problems, cp.mismatched
	if stats["written_log_failures"] > 0 {
		problems = append(problems, "the log of lines already written could not be appended; a Codex pass cut short may append some lines again")
	}
	if !w.DryRun {
		problems = append(problems, saveState(written, statePath, state, w, stats)...)
	}
	problems = append(problems, endProblems(stats, w.Invalid, mismatched, unreadable)...)
	for _, p := range problems {
		w.Error("extract: " + p)
	}
	return stats
}

// codexPass is one Run's state across the rollouts it reads.
type codexPass struct {
	stats           Stats
	w               *extract.Writer
	written         *extract.Written
	scope           *extract.Scope
	seen            map[string]bool // call ids already read this pass
	offsets, heldAt map[string]any  // the state's per-file offsets and held positions
	problems        []string
	mismatched      int // rollouts whose running total differs from the calls read
}

// file reads one rollout past its offset and writes its calls; false when a write failed and
// the pass must stop.
func (cp *codexPass) file(path string) bool {
	stats := cp.stats
	stats["files"]++
	fp, size, err := fingerprint(path)
	if err != nil {
		stats["unreadable_files"]++
		return true
	}
	if fp == "" {
		return true
	}
	start, _ := extract.Offset(cp.offsets[fp])
	if start >= size {
		return true
	}
	stats["files_read"]++
	timeouts := cp.scope.TimeoutReads
	res, err := read(path, start, cp.scope, cp.seen)
	if err != nil {
		stats["unreadable_files"]++
		return true
	}
	if cp.scope.TimeoutReads != timeouts {
		for _, id := range res.ids {
			delete(cp.seen, id)
		}
		stats["scope_held_files"]++
		return true
	}
	lines, skipped := cp.written.Filter(res.lines)
	stats["already_written"] += skipped
	if !cp.w.Write(lines) {
		cp.w.Error(fmt.Sprintf("extract: stopped — %d ledger line(s) could not be written; unwritten calls will be retried next run", cp.w.Failed))
		return false
	}
	if !cp.w.DryRun {
		if err := cp.written.Record(lines); err != nil {
			stats["written_log_failures"]++
		}
	}
	countResult(stats, res, len(lines))
	if !res.sumOK {
		cp.mismatched++
	}
	cp.offsets[fp] = res.next
	if p := noteHeld(stats, cp.heldAt, fp, res); p != "" {
		cp.problems = append(cp.problems, p)
	}
	return true
}

// openState loads the state and proves it can be written; false when the pass must hold
// (the file cannot be used, or cannot be written), said as a ledger.error and counted.
func openState(statePath string, w *extract.Writer, stats Stats) (map[string]any, bool) {
	state, err := loadState(statePath, w.DryRun)
	if err != nil {
		stats["state_unusable"]++
		w.Error(fmt.Sprintf("extract: the Codex extraction state %s %v; held: nothing extracted from Codex and the file is "+
			"left as it is, so no call is written twice. Fix or remove it (removing it reads every rollout from the start, "+
			"and calls older than the %d-day written log are written again)", stateName, err, extract.WrittenDays))
		return nil, false
	}
	if w.DryRun {
		return state, true
	}
	if err := extract.SaveJSON(statePath, state); err != nil {
		stats["state_write_failures"]++
		w.Error(fmt.Sprintf("extract: cannot write the Codex extraction state (%T); nothing extracted, so nothing is appended twice", err))
		return nil, false
	}
	return state, true
}

// loadState reads the state; a missing file (or a dry run) is the empty state. A file that is
// there and cannot be used is an error naming why: taken for empty, it would read every
// rollout from byte 0 and write again each call the written log no longer remembers.
func loadState(statePath string, dryRun bool) (map[string]any, error) {
	if dryRun {
		return map[string]any{}, nil
	}
	raw, err := os.ReadFile(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot be read (%T)", err)
	}
	v, err := extract.Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("is not JSON (%w)", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("is not a JSON object")
	}
	return m, nil
}

func countResult(stats Stats, res *fileResult, calls int) {
	stats["calls"] += calls
	stats["out_of_scope"] += res.outside
	stats["unparseable_ts"] += res.noTs
	stats["unscoped_calls"] += res.noTurn
	stats["totals_checked"] += res.checked
}

// noteHeld records where a held file is held, and says so once per held position; it
// clears the record of a file no longer held.
func noteHeld(stats Stats, heldAt map[string]any, fp string, res *fileResult) string {
	if res.drift == nil {
		delete(heldAt, fp)
		return ""
	}
	stats["held_files"]++
	at, was := extract.Offset(heldAt[fp])
	if _, present := heldAt[fp]; present && was && at == res.next {
		return ""
	}
	heldAt[fp] = res.next
	return fmt.Sprintf("a Codex rollout holds %s (Codex %s) — the rollout format may "+
		"have changed; that file is held before it, and its calls from there on are NOT captured until "+
		"Livy reads it", res.drift.what, res.drift.version)
}

// saveState compacts the written log and saves the state at the end of a pass; it returns
// the problem to report, if any.
func saveState(written *extract.Written, statePath string, state map[string]any, w *extract.Writer, stats Stats) []string {
	var problems []string
	if err := written.Compact(); err != nil {
		problems = append(problems, fmt.Sprintf("the log of lines already written could not be compacted (%T)", err))
	}
	if err := extract.SaveJSON(statePath, state); err != nil {
		stats["state_write_failures"]++
		w.Error(fmt.Sprintf("extract: the Codex extraction state could not be saved (%T); the next run may append some lines again", err))
	}
	return problems
}

// endProblems is what a pass reports about what it dropped or could not read.
func endProblems(stats Stats, invalid, mismatched, unreadable int) []string {
	var problems []string
	if invalid > 0 {
		problems = append(problems, fmt.Sprintf("%d Codex call line(s) broke the ledger line rules and were not written", invalid))
	}
	if mismatched > 0 {
		problems = append(problems, fmt.Sprintf("%d Codex rollout(s) report a running token total that differs from the "+
			"sum of the calls read — per-call usage may be incomplete", mismatched))
	}
	if stats["unscoped_calls"] > 0 {
		problems = append(problems, fmt.Sprintf("%d Codex call(s) had no turn context to scope them by and were dropped", stats["unscoped_calls"]))
	}
	if stats["unparseable_ts"] > 0 {
		problems = append(problems, fmt.Sprintf("%d Codex call(s) dropped for an unparseable timestamp", stats["unparseable_ts"]))
	}
	if stats["scope_held_files"] > 0 {
		problems = append(problems, fmt.Sprintf("%d Codex rollout(s) left unread: git timed out reading a working folder's origin; retried next run", stats["scope_held_files"]))
	}
	if stats["unreadable_files"] > 0 {
		problems = append(problems, fmt.Sprintf("%d Codex rollout(s) could not be read; retried next run", stats["unreadable_files"]))
	}
	if unreadable > 0 {
		problems = append(problems, fmt.Sprintf("%d Codex session director(y/ies) could not be listed", unreadable))
	}
	return problems
}

// fingerprint is the hash of the first complete line (a rollout's session_meta, unique per
// session), "" when there is none yet, and the file's size. State is keyed by it alone, so
// a rollout moved to another folder is not read again.
func fingerprint(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	line, err := bufio.NewReaderSize(f, 1<<16).ReadBytes('\n')
	if err != nil && err != io.EOF {
		return "", 0, err
	}
	if len(line) == 0 || line[len(line)-1] != '\n' {
		return "", st.Size(), nil
	}
	return hashHex(line), st.Size(), nil
}

// RolloutFiles lists every rollout under root, sorted.
func RolloutFiles(root string) []string {
	var unreadable int
	return files(root, &unreadable)
}

// Fingerprint is the hash of a rollout's first complete line and the file's size.
func Fingerprint(path string) (string, int64, error) { return fingerprint(path) }
