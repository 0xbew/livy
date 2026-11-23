// Package claudecode extracts facts from Claude Code's local transcripts
// (~/.claude/projects/**/*.jsonl) into the ledger: usage.api_call (one per API message, its
// final record), call.detail, interaction, tool.outcome and work.pr_linked.
//
// Facts only: counts, ids, names, flags. Prompts and tool results are read to measure or
// classify them and dropped inside the function that read them.
//
// Incremental by byte offset, per family and per file generation (a hash of the file's
// first line), so a re-run never counts a call twice. A file changed in the last
// SettleSeconds stops before the first record of the last message still open. A record this
// reader does not recognise (an unknown type, an assistant record without usage, a user
// message of an unknown shape) holds the file before it and writes a ledger.error naming
// the Claude Code version, once per held position.
package claudecode

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/extract"
)

const (
	stateName     = "extract-state.json"
	stateVersion  = 2
	saveEvery     = 25
	settleSeconds = 600
	batchLines    = 5000
)

// Families with their own offset map and carry map in state. `held` is Livy's own: where
// each file is held by a record it does not recognise, so the error is written once.
var families = []string{"files", "interactions", "interaction_carry", "details", "detail_carry", "outcomes",
	"outcome_carry", "agents", "held"}

// Stats are the run's counts, printed as JSON on stdout.
type Stats map[string]int

// Options are one run's inputs.
type Options struct {
	Roots  []string
	DryRun bool
	Dir    string // the ledger's directory, where state lives
	Stderr io.Writer
	Now    time.Time
}

// LiveRoot is $CLAUDE_CONFIG_DIR/projects, else ~/.claude/projects.
func LiveRoot() string {
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	if cfg == "" {
		home, _ := os.UserHomeDir()
		cfg = filepath.Join(home, ".claude")
	}
	return filepath.Join(cfg, "projects")
}

func pathKey(relative string) string { return hashHex(relative, 24) }

type file struct{ relative, path string }

// transcriptFiles lists every transcript and, recursively, every subagent transcript.
func transcriptFiles(root string, unreadable *int) []file {
	var found []file
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil
	}
	for _, project := range sortedListing(root, unreadable) {
		pdir := filepath.Join(root, project)
		if st, err := os.Stat(pdir); err != nil || !st.IsDir() {
			continue
		}
		for _, name := range sortedListing(pdir, unreadable) {
			found = projectEntry(root, project, name, found, unreadable)
		}
	}
	return found
}

// sortedListing is the names in dir, sorted; an unreadable dir is counted and lists nothing.
func sortedListing(dir string, unreadable *int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		*unreadable++
		return nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// projectEntry adds one project entry: a transcript file, then its subagents' transcripts.
func projectEntry(root, project, name string, found []file, unreadable *int) []file {
	full := filepath.Join(root, project, name)
	if strings.HasSuffix(name, ".jsonl") {
		if st, err := os.Stat(full); err == nil && st.Mode().IsRegular() {
			found = append(found, file{project + "/" + name, full})
		}
	}
	sub := filepath.Join(full, "subagents")
	if st, err := os.Stat(sub); err == nil && st.IsDir() {
		walk(sub, root, &found, unreadable)
	}
	return found
}

func walk(dir, root string, found *[]file, unreadable *int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		*unreadable++
		return
	}
	var dirs, files []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		} else {
			files = append(files, e.Name())
		}
	}
	sort.Strings(dirs)
	sort.Strings(files)
	for _, f := range files {
		if strings.HasSuffix(f, ".jsonl") {
			p := filepath.Join(dir, f)
			rel, _ := filepath.Rel(root, p)
			*found = append(*found, file{rel, p})
		}
	}
	for _, d := range dirs {
		walk(filepath.Join(dir, d), root, found, unreadable)
	}
}

// firstLineHash is the hash of the first complete line, "" when there is none yet.
func firstLineHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 64*1024)
	for {
		n, err := f.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if i := strings.IndexByte(string(chunk[:n]), '\n'); i >= 0 {
			line := buf[:len(buf)-n+i+1]
			return hashHex(string(line), 16), nil
		}
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", err
		}
	}
}

func loadState(path string, say func(string)) map[string]any {
	empty := func() map[string]any {
		s := map[string]any{"version": int64(stateVersion), "pr_links": []any{}}
		for _, f := range families {
			s[f] = map[string]any{}
		}
		return s
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			say(fmt.Sprintf("extract: state unreadable (%T); reading all transcripts from the start, so some lines may be appended again", err))
		}
		return empty()
	}
	v, err := extract.Decode(raw)
	if err != nil {
		say(fmt.Sprintf("extract: state unreadable (%T); reading all transcripts from the start, so some lines may be appended again", err))
		return empty()
	}
	s, ok := v.(map[string]any)
	ver, vok := extract.Int(s["version"])
	links, lok := s["pr_links"].([]any)
	_, fok := s["files"].(map[string]any)
	if !ok || !vok || ver != stateVersion || !lok || !fok {
		say("extract: state has a shape this version does not read; reading all transcripts from the start, so some lines may be appended again")
		return empty()
	}
	kept := []any{}
	for _, l := range links {
		if str, ok := l.(string); ok {
			kept = append(kept, str)
		}
	}
	s["pr_links"] = kept
	for _, f := range families {
		extract.SubMap(s, f)
	}
	return s
}

// Run is one incremental pass over every root.
func Run(o Options) Stats {
	ps, ok := newExtractPass(o)
	if !ok {
		return ps.stats
	}
	for _, root := range o.Roots {
		for _, tf := range transcriptFiles(root, &ps.unreadable) {
			if h := ps.readFile(tf); h != nil {
				return ps.stop(h.why, h.stateFailure)
			}
		}
	}
	return ps.end()
}

// extractPass is one Run: its counts, state and the ids already handled across files.
type extractPass struct {
	o            Options
	stats        Stats
	w            *extract.Writer
	statePath    string
	state        map[string]any
	written      *extract.Written
	recordFailed bool
	scope        *extract.Scope
	rd           *reader

	seenMessages, seenInteractions, seenDetails, seenOutcomes, seenLinks map[string]bool

	unreadable int
	dirty      int
	problems   []string
}

// halt is why a pass stops early; unwritten calls are retried next run.
type halt struct {
	why          string
	stateFailure bool
}

// fileLines is what one transcript adds: its ledger lines, the links it names, and how to
// forget the ids it marked seen if the file is read again next run.
type fileLines struct {
	lines    []map[string]any
	links    map[string]bool
	fileSeen []func()
}

// markSeen marks id seen in set, and remembers how to forget it.
func (fl *fileLines) markSeen(set map[string]bool, id string) {
	set[id] = true
	fl.fileSeen = append(fl.fileSeen, func() { delete(set, id) })
}

// newExtractPass loads state and the written log; false when the state cannot be written,
// so nothing is extracted and nothing is appended twice.
func newExtractPass(o Options) (*extractPass, bool) {
	ps := &extractPass{
		o:                o,
		stats:            Stats{},
		w:                &extract.Writer{Dir: o.Dir, Stderr: o.Stderr, DryRun: o.DryRun},
		statePath:        filepath.Join(o.Dir, stateName),
		seenMessages:     map[string]bool{},
		seenInteractions: map[string]bool{},
		seenDetails:      map[string]bool{},
		seenOutcomes:     map[string]bool{},
		seenLinks:        map[string]bool{},
	}
	if o.DryRun {
		ps.state = loadState("", func(string) {})
	} else {
		ps.state = loadState(ps.statePath, ps.w.Error)
		if err := extract.SaveJSON(ps.statePath, ps.state); err != nil {
			ps.stats["state_write_failures"]++
			ps.w.Error(fmt.Sprintf("extract: cannot write extraction state (%T); nothing extracted, so nothing is appended twice", err))
			return ps, false
		}
	}
	written, err := extract.LoadWritten(o.Dir)
	if err != nil {
		ps.w.Error(fmt.Sprintf("extract: the log of lines already written could not be read (%T); a pass that was cut short may append some lines again", err))
	}
	ps.written = written
	ps.scope = extract.NewScope(o.Stderr)
	ps.rd = &reader{scope: ps.scope}
	for _, l := range ps.state["pr_links"].([]any) {
		ps.seenLinks[l.(string)] = true
	}
	return ps, true
}

func (ps *extractPass) save() error {
	var links []string
	for l := range ps.seenLinks {
		links = append(links, l)
	}
	sort.Strings(links)
	ps.state["pr_links"] = stringsAny(links)
	if ps.o.DryRun {
		return nil
	}
	return extract.SaveJSON(ps.statePath, ps.state)
}

func (ps *extractPass) stop(why string, stateFailure bool) Stats {
	if stateFailure {
		ps.stats["state_write_failures"]++
	}
	ps.w.Error("extract: stopped — " + why + "; unwritten calls will be retried next run")
	_ = ps.save()
	ps.stats["unreadable_dirs"] = ps.unreadable
	report(ps.stats, ps.scope, ps.w, ps.problems)
	ps.stats["written"], ps.stats["invalid"], ps.stats["failed_writes"] = ps.w.Written, ps.w.Invalid, ps.w.Failed
	return ps.stats
}

func (ps *extractPass) end() Stats {
	ps.stats["unreadable_dirs"] = ps.unreadable
	if !ps.o.DryRun {
		if err := ps.written.Compact(); err != nil {
			ps.problems = append(ps.problems, fmt.Sprintf("the log of lines already written could not be compacted (%T)", err))
		}
		if err := ps.save(); err != nil {
			ps.stats["state_write_failures"]++
			ps.w.Error(fmt.Sprintf("extract: state could not be saved at the end (%T); the next run may append some lines again", err))
		}
	}
	report(ps.stats, ps.scope, ps.w, ps.problems)
	ps.stats["written"], ps.stats["invalid"], ps.stats["failed_writes"] = ps.w.Written, ps.w.Invalid, ps.w.Failed
	return ps.stats
}

// identify is a transcript's info and generation (its first line's hash); false when it is
// gone, unreadable or empty.
func (ps *extractPass) identify(path string) (os.FileInfo, string, bool) {
	st, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			ps.stats["unreadable_files"]++
		}
		return nil, "", false
	}
	fp, err := firstLineHash(path)
	if err != nil {
		if !os.IsNotExist(err) {
			ps.stats["unreadable_files"]++
		}
		return nil, "", false
	}
	return st, fp, fp != ""
}

// readFile reads one transcript past its offsets and writes its lines.
func (ps *extractPass) readFile(tf file) *halt {
	ps.stats["files"]++
	st, fp, ok := ps.identify(tf.path)
	if !ok {
		return nil
	}
	size := st.Size()
	key := pathKey(tf.relative)
	gen := map[string]map[string]any{}
	for _, f := range families {
		gen[f] = extract.SubMap(ps.state[f].(map[string]any), key)
	}
	off := func(f string) int64 { v, _ := extract.Offset(gen[f][fp]); return v }
	start, istart, dstart, ostart := off("files"), off("interactions"), off("details"), off("outcomes")
	if start >= size && istart >= size && dstart >= size && ostart >= size {
		return nil
	}
	ps.stats["files_read"]++
	timeoutsBefore := ps.scope.TimeoutReads
	before := maps.Clone(ps.stats)
	settled := ps.o.Now.Sub(st.ModTime()) >= settleSeconds*time.Second
	p := ps.rd.readTranscript(tf.path, start, settled, istart, gen["interaction_carry"][fp], dstart,
		gen["detail_carry"][fp], ostart, gen["outcome_carry"][fp])
	if p.readError {
		ps.stats["unreadable_files"]++
		return nil
	}
	ps.stats["bad_lines"] += p.badLines
	ps.stats["synthetic"] += p.synthetic
	fl := &fileLines{links: map[string]bool{}}
	ps.usageLines(p, fl)
	ps.linkLines(p, fl)
	ps.interactionLines(p, fl)
	ps.detailLines(p, fl)
	ps.outcomeLines(p, fl)
	if ps.scope.TimeoutReads != timeoutsBefore {
		ps.holdForScope(before, fl)
		return nil
	}
	// Lines an earlier, cut-short pass already wrote (or a copy of a call in another
	// transcript) are not written again.
	lines, skipped := ps.written.Filter(fl.lines)
	ps.stats["already_written"] += skipped
	if h := ps.writeLines(lines); h != nil {
		return h
	}
	ps.advance(gen, fp, p)
	for k := range fl.links {
		ps.seenLinks[k] = true
	}
	if len(lines) == 0 {
		return nil
	}
	ps.dirty++
	if !ps.o.DryRun && ps.dirty%saveEvery == 0 {
		if err := ps.save(); err != nil {
			return &halt{fmt.Sprintf("state could not be saved (%T)", err), true}
		}
	}
	return nil
}

// holdForScope puts the counts back and forgets the file's ids: a record's scope is
// unknown, so the file is read again next run, as if unread.
func (ps *extractPass) holdForScope(before Stats, fl *fileLines) {
	held := before["scope_held_files"] + 1
	clear(ps.stats)
	maps.Copy(ps.stats, before)
	ps.stats["scope_held_files"] = held
	for _, undo := range fl.fileSeen {
		undo()
	}
}

func (ps *extractPass) writeLines(lines []map[string]any) *halt {
	for i := 0; i < len(lines); i += batchLines {
		batch := lines[i:min(i+batchLines, len(lines))]
		if !ps.w.Write(batch) {
			return &halt{fmt.Sprintf("%d ledger line(s) could not be written", ps.w.Failed), false}
		}
		if ps.o.DryRun {
			continue
		}
		if err := ps.written.Record(batch); err != nil && !ps.recordFailed {
			ps.recordFailed = true
			ps.problems = append(ps.problems, fmt.Sprintf("the log of lines already written could not be "+
				"appended (%T); a pass cut short may append some lines again", err))
		}
	}
	return nil
}

// advance moves the file's offsets and carries to where this read reached, and notes
// whether the file is held.
func (ps *extractPass) advance(gen map[string]map[string]any, fp string, p *parsed) {
	reached := p.nextOffset
	if prior, ok := extract.Offset(gen["files"][fp]); !ok || reached > prior {
		gen["files"][fp] = reached
	}
	for _, fam := range []struct {
		offsets, carries string
		carry            map[string]any
	}{{"interactions", "interaction_carry", p.carry}, {"details", "detail_carry", p.detailCarry},
		{"outcomes", "outcome_carry", p.outcomeCarry}} {
		if prior, ok := extract.Offset(gen[fam.offsets][fp]); !ok || reached >= prior {
			gen[fam.offsets][fp] = reached
			gen[fam.carries][fp] = fam.carry
		}
	}
	if p.drift == nil {
		delete(gen["held"], fp)
		return
	}
	ps.stats["held_files"]++
	at, _ := extract.Offset(gen["held"][fp])
	if _, was := gen["held"][fp]; !was || at != reached {
		gen["held"][fp] = reached
		ps.problems = append(ps.problems, fmt.Sprintf("a transcript holds %s (Claude Code %s) — the transcript "+
			"format may have changed; that file is held before it, and its calls from there on are NOT "+
			"captured until Livy reads it", p.drift.what, p.drift.version))
	}
}

func (ps *extractPass) usageLines(p *parsed, fl *fileLines) {
	for _, k := range p.messages {
		msg := k.rec["message"].(map[string]any)
		id := msg["id"].(string)
		if ps.seenMessages[id] {
			continue
		}
		fl.markSeen(ps.seenMessages, id)
		cwd, haveCwd := cwdOf(k.rec)
		placed, in := ps.scope.Place(cwd, haveCwd, k.named)
		if !in {
			ps.stats["out_of_scope"]++
			continue
		}
		ts := extract.UTCSeconds(k.rec["timestamp"])
		if ts == "" {
			ps.stats["unparseable_ts"]++
			continue
		}
		fl.lines = append(fl.lines, targetedUsage(ps.scope, k, cwd, placed, ts))
		ps.stats["calls"]++
	}
}

func (ps *extractPass) linkLines(p *parsed, fl *fileLines) {
	for _, link := range p.prLinks {
		repo, ok := link["prRepository"].(string)
		number, nok := extract.Int(link["prNumber"])
		ts := extract.UTCSeconds(link["timestamp"])
		if !ok || !nok || number == 0 || ts == "" {
			continue
		}
		if !ps.scope.Git.InScope("https://github.com/"+repo, ps.scope.Entries) {
			continue
		}
		lk := extract.PyStr(link["sessionId"]) + "|" + repo + "|" + fmt.Sprint(number)
		if ps.seenLinks[lk] || fl.links[lk] {
			continue
		}
		fl.links[lk] = true
		line := map[string]any{"schema_version": int64(extract.SchemaVersion), "ts": ts, "event": "work.pr_linked",
			"repo": repo, "pr_repository": repo, "pr_number": number, "source": "transcript"}
		if s, ok := link["sessionId"].(string); ok {
			line["session_id"] = s
		}
		fl.lines = append(fl.lines, line)
		ps.stats["pr_links"]++
	}
}

func (ps *extractPass) interactionLines(p *parsed, fl *fileLines) {
	for _, it := range p.interactions {
		if ps.seenInteractions[it.ID] {
			continue
		}
		fl.markSeen(ps.seenInteractions, it.ID)
		if !it.Scopable {
			ps.stats["unscoped_interactions"]++
			continue
		}
		if it.Repo == "" {
			ps.stats["interactions_out_of_scope"]++
			continue
		}
		ts := extract.UTCSeconds(it.Timestamp)
		if ts == "" {
			ps.stats["interaction_unparseable_ts"]++
			continue
		}
		fl.lines = append(fl.lines, interactionLine(it, ts))
		ps.stats["interactions"]++
	}
}

func (ps *extractPass) detailLines(p *parsed, fl *fileLines) {
	for _, d := range p.details {
		if ps.seenDetails[d.MessageID] {
			continue
		}
		fl.markSeen(ps.seenDetails, d.MessageID)
		cwd, haveCwd := cwdOf(d.Fields)
		placed, in := ps.scope.Place(cwd, haveCwd, d.Named)
		if !in {
			ps.stats["details_out_of_scope"]++
			continue
		}
		ts := extract.UTCSeconds(d.Fields["timestamp"])
		if ts == "" {
			ps.stats["detail_unparseable_ts"]++
			continue
		}
		fl.lines = append(fl.lines, detailLine(d, placed, ts))
		ps.stats["call_details"]++
		ps.stats["detail_bad_names"] += len(d.BadFields) + d.BadTools
		if d.Incomplete {
			ps.stats["details_incomplete"]++
		}
	}
}

func (ps *extractPass) outcomeLines(p *parsed, fl *fileLines) {
	for _, it := range p.outcomes {
		placed, ok := ps.outcomePlace(it)
		if !ok {
			continue
		}
		if it.skip != "" {
			if it.skip == "unnamed" {
				ps.stats["outcome_unnamed"]++
			} else {
				ps.stats["outcome_bad_ids"]++
			}
			continue
		}
		if ps.seenOutcomes[it.toolUseID] {
			continue
		}
		fl.markSeen(ps.seenOutcomes, it.toolUseID)
		ts := extract.UTCSeconds(it.where["timestamp"])
		if ts == "" {
			ps.stats["outcome_unparseable_ts"]++
			continue
		}
		fl.lines = append(fl.lines, outcomeLine(it, placed, ts))
		ps.stats["tool_outcomes"]++
		if it.unnamed {
			ps.stats["outcome_unnamed"]++
		}
		if it.result.category == "unrecognised" {
			ps.stats["outcomes_unrecognised"]++
		}
	}
}

// outcomePlace is where an outcome's call ran: the repo it carried in, else its result's
// working directory; false (counted) when that is unknown or out of scope.
func (ps *extractPass) outcomePlace(it *outcomeItem) (extract.Placed, bool) {
	cwd, haveCwd := cwdOf(it.where)
	if it.carriedRepo == "" && !haveCwd {
		ps.stats["unscoped_outcomes"]++
		return extract.Placed{}, false
	}
	if it.carriedRepo != "" {
		return extract.Placed{Repo: it.carriedRepo, KeepBranch: true}, true
	}
	placed, in := ps.scope.Place(cwd, haveCwd, []*extract.Named{it.named})
	if !in {
		ps.stats["outcomes_out_of_scope"]++
		return extract.Placed{}, false
	}
	return placed, true
}

func report(stats Stats, scope *extract.Scope, w *extract.Writer, problems []string) {
	var reasons []string
	for r := range scope.Problems {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	var out []string
	for _, r := range reasons {
		out = append(out, fmt.Sprintf("%d call(s): %s", scope.Problems[r], r))
	}
	scope.Problems = map[string]int{}
	out = append(out, problems...)
	say := func(n int, format string) {
		if n > 0 {
			out = append(out, fmt.Sprintf(format, n))
		}
	}
	say(stats["unparseable_ts"], "%d call(s) dropped for an unparseable timestamp")
	say(stats["interaction_unparseable_ts"], "%d interaction(s) dropped for an unparseable timestamp")
	say(stats["unscoped_interactions"], "%d interaction(s) had no working directory to scope them by and were dropped — not known to be out of scope, just unknowable")
	say(stats["detail_unparseable_ts"], "%d call detail(s) dropped for an unparseable timestamp")
	say(stats["detail_bad_names"], "%d name(s) in call details (tool, skill, subagent type, error, stop reason, effort) were missing or not a plain name, and were left out")
	say(stats["details_incomplete"], "%d call detail(s) have a tool list that could not be read in full and are marked tools_incomplete — the transcript format may have changed")
	say(stats["outcomes_unrecognised"], "%d guarded tool call(s) failed with a result this extractor does not recognise as a run or a refusal — the harness may have a new message; they are recorded as tool.outcome `unknown`, never as `ran`")
	say(stats["unscoped_outcomes"], "%d tool result(s) had no working directory to scope them by and were dropped — not known to be out of scope, just unknowable")
	say(stats["outcome_unparseable_ts"], "%d tool outcome(s) dropped for an unparseable timestamp")
	if stats["outcome_unnamed"] > 0 || stats["outcome_bad_ids"] > 0 {
		out = append(out, fmt.Sprintf("%d tool result(s) could not be matched to the call that made them, and %d had no "+
			"usable tool_use id — a guarded call among them reads as outcome unknown", stats["outcome_unnamed"], stats["outcome_bad_ids"]))
	}
	if stats["scope_held_files"] > 0 {
		out = append(out, fmt.Sprintf("%d working director(y/ies) could not be scoped: git timed out reading the origin, so %d "+
			"transcript file(s) holding their records were left unread; retried next run", len(scope.TimedOutDirs), stats["scope_held_files"]))
	}
	say(stats["bad_lines"], "%d transcript line(s) were not JSON objects")
	say(w.Invalid, "%d extracted line(s) broke the ledger line rules and were not written")
	say(w.Failed, "%d ledger line(s) failed to write and will be retried")
	say(stats["unreadable_files"], "%d transcript file(s) could not be read; they are retried next run and their calls are not captured until then")
	say(stats["unreadable_dirs"], "%d transcript director(y/ies) could not be listed")
	for _, p := range out {
		w.Error("extract: " + p)
	}
}

func base(event, repo, ts string) map[string]any {
	return map[string]any{"schema_version": int64(extract.SchemaVersion), "ts": ts, "event": event, "repo": repo,
		"source": "transcript"}
}

func branchOf(fields map[string]any, placed extract.Placed) string {
	if b, ok := extract.Str(fields["gitBranch"]); ok && placed.KeepBranch {
		return b
	}
	return extract.Unknown
}

func setStr(line map[string]any, field string, v any) {
	if s, ok := extract.Str(v); ok {
		line[field] = s
	}
}

func usageLine(rec map[string]any, placed extract.Placed, ts string) map[string]any {
	msg := rec["message"].(map[string]any)
	usage, _ := msg["usage"].(map[string]any)
	line := base("usage.api_call", placed.Repo, ts)
	line["branch"] = branchOf(rec, placed)
	line["message_id"] = msg["id"]
	for _, kv := range [][2]string{{"sessionId", "session_id"}, {"requestId", "request_id"}, {"version", "cc_version"},
		{"agentId", "agent_id"}, {"effort", "effort"}} {
		setStr(line, kv[1], rec[kv[0]])
	}
	setStr(line, "model", msg["model"])
	if b, ok := rec["isSidechain"].(bool); ok {
		line["is_sidechain"] = b
	}
	for _, k := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		if v, ok := extract.Int(usage[k]); ok {
			line[k] = v
		}
	}
	nestedCounts(line, usage)
	for _, k := range []string{"service_tier", "speed"} {
		setStr(line, k, usage[k])
	}
	return line
}

// nestedCounts copies the counts usage nests: cache creation by lifetime, thinking tokens and
// server tool requests.
func nestedCounts(line, usage map[string]any) {
	if cc, ok := usage["cache_creation"].(map[string]any); ok {
		for _, kv := range [][2]string{{"ephemeral_5m_input_tokens", "cache_creation_5m_input_tokens"},
			{"ephemeral_1h_input_tokens", "cache_creation_1h_input_tokens"}} {
			if v, ok := extract.Int(cc[kv[0]]); ok {
				line[kv[1]] = v
			}
		}
	}
	if d, ok := usage["output_tokens_details"].(map[string]any); ok {
		if v, ok := extract.Int(d["thinking_tokens"]); ok {
			line["thinking_tokens"] = v
		}
	}
	if s, ok := usage["server_tool_use"].(map[string]any); ok {
		for _, k := range []string{"web_search_requests", "web_fetch_requests"} {
			if v, ok := extract.Int(s[k]); ok {
				line[k] = v
			}
		}
	}
}

// targetedUsage is a call's usage line with where it worked: target_via, target_repo and
// target_branch.
func targetedUsage(scope *extract.Scope, k kept, cwd string, placed extract.Placed, ts string) map[string]any {
	line := usageLine(k.rec, placed, ts)
	when := extract.UTCInstant(ts)
	for f, v := range scope.TargetFields(k.named, cwd, placed.Repo, line["branch"].(string), when) {
		line[f] = v
	}
	return line
}

func interactionLine(it interaction, ts string) map[string]any {
	line := base("interaction", it.Repo, ts)
	line["kind"] = it.Kind
	line["interaction_id"] = it.ID
	line["branch"] = it.Branch
	if it.Branch == "" {
		line["branch"] = extract.Unknown
	}
	setStr(line, "session_id", it.SessionID)
	setStr(line, "cc_version", it.Version)
	if it.PromptChars != nil {
		line["prompt_chars"] = int64(*it.PromptChars)
	}
	if it.PermissionMode != "" {
		line["permission_mode"] = it.PermissionMode
	}
	if it.AfterError != nil {
		line["after_error"] = *it.AfterError
	}
	if it.SlashCommand != nil {
		line["slash_command"] = *it.SlashCommand
	}
	if it.PlanModeAction != "" {
		line["plan_mode_action"] = it.PlanModeAction
	}
	return line
}

func detailLine(d *detail, placed extract.Placed, ts string) map[string]any {
	line := base("call.detail", placed.Repo, ts)
	line["branch"] = branchOf(d.Fields, placed)
	line["message_id"] = d.MessageID
	tools := make([]any, 0, len(d.Tools))
	for _, t := range d.Tools {
		tools = append(tools, t)
	}
	line["tools"] = tools
	if d.Incomplete {
		line["tools_incomplete"] = true
	}
	setStr(line, "session_id", d.Fields["sessionId"])
	setStr(line, "agent_id", d.Fields["agentId"])
	setStr(line, "stop_reason", d.StopReason)
	setStr(line, "prompt_interaction_id", d.Prompt)
	if d.Error != nil {
		setStr(line, "api_error", *d.Error)
	}
	for _, kv := range [][2]string{{"attributionAgent", "subagent_type"}, {"attributionSkill", "skill"}, {"perTurnEffort", "per_turn_effort"}} {
		if v := d.Extra[kv[0]]; v != nil {
			setStr(line, kv[1], *v)
		}
	}
	if b, ok := d.Fields["isSidechain"].(bool); ok {
		line["is_sidechain"] = b
	}
	return line
}

func outcomeLine(it *outcomeItem, placed extract.Placed, ts string) map[string]any {
	line := base("tool.outcome", placed.Repo, ts)
	line["branch"] = branchOf(it.where, placed)
	line["tool_use_id"] = it.toolUseID
	line["outcome"] = it.result.outcome
	setStr(line, "tool", it.tool)
	setStr(line, "block_category", it.result.category)
	setStr(line, "harness_reason", it.result.reason)
	setStr(line, "session_id", it.where["sessionId"])
	setStr(line, "agent_id", it.where["agentId"])
	setStr(line, "cc_version", it.where["version"])
	if b, ok := it.where["isSidechain"].(bool); ok {
		line["is_sidechain"] = b
	}
	return line
}

// MarshalStats is the run's counts as sorted JSON.
func MarshalStats(s Stats) string {
	b, _ := json.Marshal(map[string]int(s))
	return string(b)
}
