package claudecode

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/extract"
	"github.com/0xbew/livy/internal/ledgerjson"
	"github.com/0xbew/livy/internal/oldnames"
	"github.com/0xbew/livy/internal/pytext"
)

// The tool-call backfill: one `tool.call` line per tool call in the local transcripts, so a
// PR's tool history reaches back before tool spans were exported. Structure only: names,
// ids, instants and byte sizes. A call's input and its result are measured while read and
// dropped, never held and never written.
//
// It keeps its own state file and lock, so it can be re-run: an offset moves only once
// every line read from that file has reached the ledger.
const (
	// ToolBackfillLockName is the backfill's own lock, apart from the extract pass's.
	ToolBackfillLockName = "tool-backfill.lock"

	toolStateName    = "tool-backfill-state.json"
	toolStateVersion = 1
	backfillSource   = "transcript-backfill"
	// A resultless call younger than this may still be running, so its file waits for the next run.
	defaultReplaySeconds = 86400.0
	replayEnv            = "LIVY_TOOL_BACKFILL_REPLAY_SECONDS"
)

// legacyReplayEnv is the Python tool's name for replayEnv; "" in a build with none.
var legacyReplayEnv = oldnames.LauncherEnv("TOOL_BACKFILL_REPLAY_SECONDS")

// DataNames are the files and folders this package keeps in the data folder, by exact name.
// `livy uninstall --purge` deletes these and nothing else.
func DataNames() []string { return []string{stateName, toolStateName, ToolBackfillLockName} }

// A line without any of these cannot hold a tool call, its result, or a stop reason.
var toolMarkers = [][]byte{[]byte("tool_use"), []byte("tool_result"), []byte("stop_reason")}

// Counters that say something was lost or could not be read; each is reported, in this order.
var toolProblemCounters = []string{"bad_tool_use", "bad_result_id", "unparseable_ts", "tool_stop_without_tool_use",
	"orphan_results", "unscoped", "bad_lines", "invalid", "failed_writes", "unreadable_files",
	"unreadable_dirs", "negative_durations", "scope_held_files"}

func replaySeconds() float64 {
	for _, name := range []string{replayEnv, legacyReplayEnv} {
		if v, set := os.LookupEnv(name); name != "" && set {
			// An infinite window would hold every file with a resultless call forever.
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && f > 0 {
				return f
			}
			return defaultReplaySeconds
		}
	}
	return defaultReplaySeconds
}

type toolCall struct {
	id, tool, message string
	timestamp         any
	cwd               string
	haveCwd           bool
	branch, session   string
	agent, version    string
	sidechain         *bool
	inputBytes        int
	named             *extract.Named
}

type toolResult struct {
	timestamp   any
	isError     bool
	outputBytes int
	spawned     string
}

type toolRead struct {
	calls                                 map[string]*toolCall
	order                                 []string
	results                               map[string]*toolResult
	nextOffset                            int64
	readError                             bool
	badLines, badToolUse, badResultID     int
	repeatedToolUse, toolStopWithoutBlock int
	badVersions                           map[string]bool
}

// sizeOf is the UTF-8 length of a string, or of the compact JSON of anything else, as Python measures it.
func sizeOf(v any) int {
	if s, ok := v.(string); ok {
		return len(s)
	}
	b, _ := ledgerjson.MarshalUTF8(v)
	return len(b)
}

func trimmedText(v any) string {
	if s, ok := v.(string); ok && pytext.Strip(s) != "" {
		return s
	}
	return ""
}

// readCalls reads calls and results from start to the last complete line.
func readCalls(path string, start int64) *toolRead {
	out := &toolRead{calls: map[string]*toolCall{}, results: map[string]*toolResult{}, nextOffset: start,
		badVersions: map[string]bool{}}
	messages := map[string]*msgInfo{}
	f, err := os.Open(path)
	if err != nil {
		out.readError = true
		return out
	}
	defer f.Close()
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		out.readError = true
		return out
	}
	br := bufio.NewReaderSize(f, 1<<20)
	offset := start
	for {
		raw, err := br.ReadBytes('\n')
		if err != nil && err != io.EOF {
			out.readError = true
			return out
		}
		if len(raw) == 0 || raw[len(raw)-1] != '\n' {
			break // a line still being written: next run
		}
		offset += int64(len(raw))
		if !hasToolMarker(raw) {
			continue
		}
		v, derr := extract.Decode(raw)
		rec, isMap := v.(map[string]any)
		if derr != nil || !isMap {
			out.badLines++
			continue
		}
		readToolRecord(rec, out, messages)
	}
	out.nextOffset = offset
	out.countStopsWithoutBlock(messages)
	return out
}

// hasToolMarker is whether a raw line can hold a tool call or result; others are skipped
// undecoded.
func hasToolMarker(raw []byte) bool {
	for _, m := range toolMarkers {
		if bytes.Contains(raw, m) {
			return true
		}
	}
	return false
}

// countStopsWithoutBlock counts messages that stopped for a tool call yet carried none, and
// the Claude Code versions that wrote them.
func (out *toolRead) countStopsWithoutBlock(messages map[string]*msgInfo) {
	for _, info := range messages {
		if info.stop && !info.block {
			out.toolStopWithoutBlock++
			if info.version != "" {
				out.badVersions[info.version] = true
			}
		}
	}
}

// msgInfo is what one assistant message id has shown so far: a tool stop reason, a tool_use block.
// Records with no id share one entry, as they do in the Python reader.
type msgInfo struct {
	stop, block bool
	version     string
}

func readToolRecord(rec map[string]any, out *toolRead, messages map[string]*msgInfo) {
	msg, ok := rec["message"].(map[string]any)
	if !ok {
		return
	}
	switch rec["type"] {
	case "assistant":
		readToolUses(rec, msg, out, messages)
	case "user":
		readToolResults(rec, msg, out)
	}
}

func readToolUses(rec, msg map[string]any, out *toolRead, messages map[string]*msgInfo) {
	version := trimmedText(rec["version"])
	mid := trimmedText(msg["id"])
	info := messages[mid]
	if info == nil {
		info = &msgInfo{}
		messages[mid] = info
	}
	if msg["stop_reason"] == "tool_use" {
		info.stop = true
		if info.version == "" {
			info.version = version
		}
	}
	blocks, ok := msg["content"].([]any)
	if !ok {
		return
	}
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok || block["type"] != "tool_use" {
			continue
		}
		info.block = true
		id, idOK := block["id"].(string)
		name, nameOK := block["name"].(string)
		if !idOK || !toolIDRe.MatchString(id) || !nameOK || !extract.IsName(name) {
			out.badToolUse++
			if version != "" {
				out.badVersions[version] = true
			}
			continue
		}
		if _, seen := out.calls[id]; seen {
			out.repeatedToolUse++
			continue
		}
		out.calls[id] = newToolCall(rec, msg, block, id, name)
		out.order = append(out.order, id)
	}
}

func newToolCall(rec, msg, block map[string]any, id, name string) *toolCall {
	call := &toolCall{id: id, tool: name, timestamp: rec["timestamp"], branch: trimmedText(rec["gitBranch"]),
		session: trimmedText(rec["sessionId"]), agent: trimmedText(rec["agentId"]), version: trimmedText(rec["version"]),
		message: trimmedText(msg["id"]), inputBytes: sizeOf(block["input"])}
	call.cwd, call.haveCwd = cwdOf(rec)
	if sc, ok := rec["isSidechain"].(bool); ok {
		call.sidechain = &sc
	}
	call.named = extract.NamedBy(name, block["input"], call.cwd)
	return call
}

func readToolResults(rec, msg map[string]any, out *toolRead) {
	items, ok := msg["content"].([]any)
	if !ok {
		return
	}
	var blocks []map[string]any
	for _, b := range items {
		if block, ok := b.(map[string]any); ok && block["type"] == "tool_result" {
			blocks = append(blocks, block)
		}
	}
	spawned := spawnedAgent(rec, len(blocks))
	for _, block := range blocks {
		id, ok := block["tool_use_id"].(string)
		if !ok || !toolIDRe.MatchString(id) {
			out.badResultID++
			continue
		}
		if _, seen := out.results[id]; seen {
			continue
		}
		out.results[id] = &toolResult{timestamp: rec["timestamp"], isError: block["is_error"] == true,
			outputBytes: sizeOf(block["content"]), spawned: spawned}
	}
}

// spawnedAgent is the agent a record's one tool result started, or "".
func spawnedAgent(rec map[string]any, results int) string {
	meta, ok := rec["toolUseResult"].(map[string]any)
	if !ok || results != 1 {
		// With several results in one record there is no telling which one spawned the agent.
		return ""
	}
	if s, ok := meta["agentId"].(string); ok && toolIDRe.MatchString(s) {
		return s
	}
	return ""
}

func isoMillis(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000") + "+00:00" }

// toolCallLine is the tool.call line for one call, and its result if one came.
func toolCallLine(c *toolCall, res *toolResult, repo string, keepBranch bool, scope *extract.Scope,
	start time.Time, stats Stats) map[string]any {
	branch := extract.Unknown
	if keepBranch && c.branch != "" {
		branch = c.branch
	}
	line := base("tool.call", repo, extract.UTCSeconds(c.timestamp))
	line["source"] = backfillSource
	line["branch"] = branch
	line["tool_use_id"] = c.id
	line["tool"] = c.tool
	line["start_ts"] = isoMillis(start)
	line["result"] = "no_result"
	line["input_bytes"] = int64(c.inputBytes)
	setStr(line, "session_id", c.session)
	setStr(line, "agent_id", c.agent)
	setStr(line, "cc_version", c.version)
	setStr(line, "message_id", c.message)
	if c.sidechain != nil {
		line["is_sidechain"] = *c.sidechain
	}
	for k, v := range scope.TargetFields([]*extract.Named{c.named}, c.cwd, repo, branch, &start) {
		line[k] = v
	}
	if res == nil {
		return line
	}
	line["result"] = "ok"
	if res.isError {
		line["result"] = "error"
	}
	line["output_bytes"] = int64(res.outputBytes)
	setStr(line, "spawned_agent_id", res.spawned)
	end := extract.UTCInstant(res.timestamp)
	if end == nil {
		stats["unparseable_ts"]++
		return line
	}
	millis := float64(end.Sub(start).Microseconds())/1e6*1000 + 0.5
	if end.Before(start) {
		millis -= 1
	}
	if millis := int64(millis); millis >= 0 {
		line["end_ts"] = isoMillis(*end)
		line["duration_ms"] = millis
	} else {
		stats["negative_durations"]++
	}
	return line
}

func toolReport(stats Stats, versions map[string]bool, scope *extract.Scope, w *extract.Writer) {
	var vs []string
	for v := range versions {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	seen := strings.Join(vs, ", ")
	if seen == "" {
		seen = "unknown"
	}
	wording := map[string]string{
		"bad_tool_use": fmt.Sprintf("%d tool_use block(s) had no usable id or name (Claude Code %s) "+
			"— the transcript format may have changed; those calls are NOT captured", stats["bad_tool_use"], seen),
		"tool_stop_without_tool_use": fmt.Sprintf("%d assistant message(s) stopped for a tool call but carried no "+
			"tool_use block (Claude Code %s) — the transcript format may have changed; those calls are NOT captured",
			stats["tool_stop_without_tool_use"], seen),
		"bad_result_id": fmt.Sprintf("%d tool result(s) had no usable tool_use id", stats["bad_result_id"]),
		"orphan_results": fmt.Sprintf("%d tool result(s) had no matching call in the same pass and were dropped",
			stats["orphan_results"]),
		"unscoped": fmt.Sprintf("%d tool call(s) had no working directory to scope them by and were NOT captured",
			stats["unscoped"]),
		"unparseable_ts": fmt.Sprintf("%d timestamp(s) were unparseable; those calls are dropped or carry no end time",
			stats["unparseable_ts"]),
		"negative_durations": fmt.Sprintf("%d result(s) were stamped before their call; those calls carry no end time or duration",
			stats["negative_durations"]),
		"bad_lines":        fmt.Sprintf("%d transcript line(s) were not JSON objects", stats["bad_lines"]),
		"invalid":          fmt.Sprintf("%d line(s) broke the ledger line rules and were not written", stats["invalid"]),
		"failed_writes":    fmt.Sprintf("%d ledger line(s) failed to write and will be retried", stats["failed_writes"]),
		"unreadable_files": fmt.Sprintf("%d transcript file(s) could not be read; retried next run", stats["unreadable_files"]),
		"unreadable_dirs":  fmt.Sprintf("%d transcript director(y/ies) could not be listed", stats["unreadable_dirs"]),
		"scope_held_files": fmt.Sprintf("%d transcript file(s) were left unread: git timed out after %gs reading a "+
			"working directory's origin, so its calls' scope is unknown; retried next run",
			stats["scope_held_files"], scope.Git.Timeout()),
	}
	var reasons []string
	for _, key := range toolProblemCounters {
		if stats[key] > 0 {
			reasons = append(reasons, wording[key])
		}
	}
	var targets []string
	for r := range scope.Problems {
		targets = append(targets, r)
	}
	sort.Strings(targets)
	for _, r := range targets {
		reasons = append(reasons, fmt.Sprintf("%d call(s): %s", scope.Problems[r], r))
	}
	scope.Problems = map[string]int{}
	for _, problem := range reasons {
		w.Error("tool-backfill: " + problem)
	}
}

func loadToolState(dir string, say func(string)) map[string]any {
	empty := func() map[string]any {
		return map[string]any{"version": int64(toolStateVersion), "files": map[string]any{}}
	}
	raw, err := os.ReadFile(filepath.Join(dir, toolStateName))
	if err != nil {
		if !os.IsNotExist(err) {
			say(fmt.Sprintf("tool-backfill: state unreadable (%T); reading all transcripts from the start", err))
		}
		return empty()
	}
	v, err := extract.Decode(raw)
	if err != nil {
		say(fmt.Sprintf("tool-backfill: state unreadable (%T); reading all transcripts from the start", err))
		return empty()
	}
	s, ok := v.(map[string]any)
	ver, vok := extract.Int(s["version"])
	_, fok := s["files"].(map[string]any)
	if !ok || !vok || ver != toolStateVersion || !fok {
		return empty()
	}
	return s
}

// ToolBackfill is one backfill pass over every root.
func ToolBackfill(o Options) Stats {
	tp, ok := newToolPass(o)
	if !ok {
		return tp.stats
	}
	for _, root := range o.Roots {
		abs, _ := filepath.Abs(root)
		for _, tf := range transcriptFiles(root, &tp.unreadable) {
			if why := tp.readFile(abs, tf); why != "" {
				return tp.finish(why)
			}
		}
	}
	return tp.finish("")
}

// toolPass is one ToolBackfill: its counts, state and the call ids written across files.
type toolPass struct {
	o          Options
	w          *extract.Writer
	statePath  string
	state      map[string]any
	files      map[string]any
	stats      Stats
	scope      *extract.Scope
	seen       map[string]bool
	horizon    time.Duration
	versions   map[string]bool
	unreadable int
	dirty      int
}

// newToolPass loads state; false when it cannot be written, so nothing is appended.
func newToolPass(o Options) (*toolPass, bool) {
	stderr := o.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	tp := &toolPass{
		o:         o,
		w:         &extract.Writer{Dir: o.Dir, Stderr: stderr, DryRun: o.DryRun},
		statePath: filepath.Join(o.Dir, toolStateName),
		state:     map[string]any{"version": int64(toolStateVersion), "files": map[string]any{}},
		stats:     Stats{},
	}
	if !o.DryRun {
		tp.state = loadToolState(o.Dir, func(s string) { fmt.Fprintln(stderr, s) })
	}
	for _, k := range append([]string{"files", "files_read", "unsettled_files", "held_files", "tool_calls",
		"no_result_calls", "out_of_scope", "unscoped", "duplicate_across_files", "repeated_tool_use",
		"negative_durations", "written", "state_write_failures"}, toolProblemCounters...) {
		tp.stats[k] = 0
	}
	if !o.DryRun {
		if err := extract.SaveJSON(tp.statePath, tp.state); err != nil { // prove state is writable BEFORE appending anything
			tp.stats["state_write_failures"]++
			tp.w.Error(fmt.Sprintf("tool-backfill: cannot write state (%T); nothing appended", err))
			return tp, false
		}
	}
	tp.files = extract.SubMap(tp.state, "files")
	tp.scope = extract.NewScope(stderr)
	tp.seen = map[string]bool{}
	// Clamped: a window past what a Duration holds would overflow and stop holding any file.
	tp.horizon = time.Duration(math.Min(replaySeconds(), float64(math.MaxInt64/int64(time.Second))) * float64(time.Second))
	tp.versions = map[string]bool{}
	return tp, true
}

// finish saves state and reports; why is the reason the pass stopped early, or "".
func (tp *toolPass) finish(why string) Stats {
	tp.stats["unreadable_dirs"] = tp.unreadable
	if !tp.o.DryRun {
		if err := extract.SaveJSON(tp.statePath, tp.state); err != nil {
			tp.stats["state_write_failures"]++
			if why == "" {
				tp.w.Error(fmt.Sprintf("tool-backfill: state could not be saved at the end (%T); the next run may append some lines again", err))
			}
		}
	}
	if why != "" {
		tp.w.Error(fmt.Sprintf("tool-backfill: stopped — %s; unwritten calls will be retried next run", why))
	}
	tp.stats["written"], tp.stats["invalid"], tp.stats["failed_writes"] = tp.w.Written, tp.w.Invalid, tp.w.Failed
	toolReport(tp.stats, tp.versions, tp.scope, tp.w)
	return tp.stats
}

// settledFile is a transcript's info and generation; false when it is gone, unreadable,
// empty or still changing.
func (tp *toolPass) settledFile(path string) (os.FileInfo, string, bool) {
	st, err := os.Stat(path)
	var fp string
	if err == nil {
		fp, err = firstLineHash(path)
	}
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			tp.stats["unreadable_files"]++
		}
		return nil, "", false
	}
	if fp == "" {
		return nil, "", false
	}
	if tp.o.Now.Sub(st.ModTime()) < settleSeconds*time.Second {
		tp.stats["unsettled_files"]++
		return nil, "", false
	}
	return st, fp, true
}

// readFile reads one transcript of the root at abs past its offset and writes its calls;
// it returns why the pass must stop, or "".
func (tp *toolPass) readFile(abs string, tf file) string {
	tp.stats["files"]++
	st, fp, ok := tp.settledFile(tf.path)
	if !ok {
		return ""
	}
	// Per root: copies of one transcript in two roots differ in length, and a shared offset would
	// start the longer one after a call only the shorter one holds.
	perFile := extract.SubMap(tp.files, pathKey(abs+"\x00"+tf.relative))
	prior, havePrior := extract.Offset(perFile[fp])
	if prior >= st.Size() {
		return ""
	}
	tp.stats["files_read"]++
	parsed := readCalls(tf.path, prior)
	if parsed.readError {
		tp.stats["unreadable_files"]++
		return ""
	}
	if hasRunningCall(parsed, tp.o.Now, tp.horizon) {
		tp.stats["held_files"]++ // nothing counted or written: the next run reads this range again
		return ""
	}
	timeoutsBefore := tp.scope.TimeoutReads
	before := maps.Clone(tp.stats)
	tp.countRead(parsed)
	lines, fileSeen := tp.callLines(parsed, timeoutsBefore)
	if tp.scope.TimeoutReads != timeoutsBefore {
		tp.holdForScope(before, fileSeen)
		return ""
	}
	for i := 0; i < len(lines); i += batchLines {
		batch := lines[i:min(i+batchLines, len(lines))]
		failedBefore := tp.w.Failed
		if !tp.w.Write(batch) {
			return fmt.Sprintf("%d ledger line(s) could not be written", tp.w.Failed-failedBefore)
		}
	}
	// Never backwards: a shorter copy of a generation must not undo a longer read.
	reached := parsed.nextOffset
	if havePrior && prior > reached {
		reached = prior
	}
	perFile[fp] = reached
	return tp.checkpoint(len(lines))
}

// checkpoint saves state every saveEvery files that wrote lines; it returns why the pass
// must stop, or "".
func (tp *toolPass) checkpoint(written int) string {
	if written == 0 {
		return ""
	}
	tp.dirty++
	if tp.o.DryRun || tp.dirty%saveEvery != 0 {
		return ""
	}
	if err := extract.SaveJSON(tp.statePath, tp.state); err != nil {
		tp.stats["state_write_failures"]++
		return fmt.Sprintf("state could not be saved (%T)", err)
	}
	return ""
}

// countRead adds what the read found wrong to the counts.
func (tp *toolPass) countRead(parsed *toolRead) {
	tp.stats["bad_lines"] += parsed.badLines
	tp.stats["bad_tool_use"] += parsed.badToolUse
	tp.stats["bad_result_id"] += parsed.badResultID
	tp.stats["repeated_tool_use"] += parsed.repeatedToolUse
	tp.stats["tool_stop_without_tool_use"] += parsed.toolStopWithoutBlock
	for v := range parsed.badVersions {
		tp.versions[v] = true
	}
	for id := range parsed.results {
		if parsed.calls[id] == nil {
			tp.stats["orphan_results"]++
		}
	}
}

// callLines is the file's tool.call lines and the ids it marked seen; it stops at the first
// call whose scope timed out.
func (tp *toolPass) callLines(parsed *toolRead, timeoutsBefore int) ([]map[string]any, []string) {
	var lines []map[string]any
	var fileSeen []string
	for _, id := range parsed.order {
		c := parsed.calls[id]
		if tp.seen[id] {
			tp.stats["duplicate_across_files"]++ // information only: every copy is written, derive picks
		}
		if !c.haveCwd {
			tp.stats["unscoped"]++
			continue
		}
		placed, in := tp.scope.Place(c.cwd, true, []*extract.Named{c.named})
		if !in && tp.scope.TimeoutReads != timeoutsBefore {
			break
		}
		if !in {
			tp.stats["out_of_scope"]++
			continue
		}
		if !tp.seen[id] {
			tp.seen[id] = true
			fileSeen = append(fileSeen, id)
		}
		start := extract.UTCInstant(c.timestamp)
		if start == nil {
			tp.stats["unparseable_ts"]++
			continue
		}
		res := parsed.results[id]
		lines = append(lines, toolCallLine(c, res, placed.Repo, placed.KeepBranch, tp.scope, *start, tp.stats))
		tp.stats["tool_calls"]++
		if res == nil {
			tp.stats["no_result_calls"]++
		}
	}
	return lines, fileSeen
}

// holdForScope puts the counts back and forgets the file's ids: a call's scope is unknown,
// so the file is read again next run, as if this pass had not read it.
func (tp *toolPass) holdForScope(before Stats, fileSeen []string) {
	held := before["scope_held_files"] + 1
	clear(tp.stats)
	maps.Copy(tp.stats, before)
	tp.stats["scope_held_files"] = held
	for _, id := range fileSeen {
		delete(tp.seen, id)
	}
}

// hasRunningCall is true for a call with no result yet and a start younger than the horizon:
// its result may still arrive.
func hasRunningCall(p *toolRead, now time.Time, horizon time.Duration) bool {
	for _, id := range p.order {
		if p.results[id] != nil {
			continue
		}
		// A start after now (a skewed clock) would stay young, and hold the file, for good.
		if started := extract.UTCInstant(p.calls[id].timestamp); started != nil {
			if age := now.Sub(*started); age >= 0 && age < horizon {
				return true
			}
		}
	}
	return false
}
