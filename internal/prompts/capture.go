// Package prompts is prompt capture and prompt upload: one text-free prompt.captured ledger
// line per new human prompt in an in-scope repo, and, only when the engineer has turned
// upload on, its scrubbed and audited text record in a local queue that emit ships to the
// prompt sink. Claude Code transcripts and Codex rollouts both feed it.
//
// Never written: a prompt from a session out of scope, a turn a program sent, a relay whose
// proven source is outside the configured orgs (outside_relay), and any context field the
// relay rule attributes to an outside source, which becomes Withheld. A record the audit
// refuses is never queued: its event says refused_audit and a ledger.error names the prompt
// id and the rule ids, never the text.
//
// Prompts seen while upload is off are not uploaded later on their own: only a backfill run
// with upload on sends them. The first pass on a machine starts at the end of every transcript.
package prompts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/0xbew/scrub"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/extract"
	"github.com/0xbew/livy/internal/extract/claudecode"
	"github.com/0xbew/livy/internal/extract/codex"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/ticket"
	"github.com/0xbew/livy/internal/vault"
)

// Names inside the data folder. The queue's names match the Python tool's, so a folder it
// left behind ships under the same keys.
const (
	StateName    = "prompt-capture-state.json"
	TextFile     = "prompt-text.jsonl"
	TextLock     = "prompt-text.lock"
	SegmentsDir  = "prompt-segments"
	RejectedDir  = "prompt-rejected"
	stateVersion = 1
	// PassEvery is how seldom the pass runs on its own: proving a relay reads two weeks of
	// every project's transcripts.
	PassEvery = time.Hour
	// A prompt id stays in state long enough to dedupe any backfill of the window, then ages out.
	capturedKeepDays = 120
	capturedKeep     = capturedKeepDays * 24 * time.Hour
)

// DataNames are the files and folders this package keeps in the data folder, by exact name.
// `livy uninstall --purge` deletes these and nothing else.
func DataNames() []string {
	return []string{StateName, TextFile, TextLock, SegmentsDir, RejectedDir, DenylistFile, extractLock, vault.Dir}
}

// resendable is what a backfill may send again: what was never sent, and not because the
// audit or the relay rule refused it.
var resendable = map[string]bool{"upload_off": true, "no_denylist": true, "error": true}

// Options are one pass's inputs.
type Options struct {
	Roots        []string // Claude Code transcript roots
	CodexRoot    string
	DryRun       bool
	BackfillDays *int
	Now          time.Time
	Dir          string
	Stderr       io.Writer
}

type fileState struct {
	FP     string
	Offset int64
}

type state struct {
	Files    map[string]fileState
	Captured map[string]captured
	LastPass float64
	// Drifted is each file held at a record the reader does not know, by the offset it was
	// held at: an offset already said is not said again.
	Drifted map[string]int64
}

type captured struct {
	At     float64
	Status string
}

// loadState reads the state; a missing file is the empty state of a first pass. A file that
// is there and cannot be used is an error naming why: taking it for a first pass would move
// every transcript to its end and skip each prompt since the last good pass.
func loadState(dir string) (state, error) {
	st := state{Files: map[string]fileState{}, Captured: map[string]captured{}, Drifted: map[string]int64{}}
	raw, err := os.ReadFile(filepath.Join(dir, StateName))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("cannot be read (%T)", err)
	}
	var top map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil {
		return st, fmt.Errorf("is not a JSON object (%w)", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return st, errors.New("has data after its JSON object")
	}
	n, _ := top["version"].(json.Number)
	if n == "" {
		return st, fmt.Errorf("has no version number; this Livy reads version %d", stateVersion)
	}
	if n != json.Number(fmt.Sprint(stateVersion)) {
		return st, fmt.Errorf("has version %s; this Livy reads version %d", n, stateVersion)
	}
	if m, ok := top["files"].(map[string]any); ok {
		for k, v := range m {
			e, _ := v.(map[string]any)
			fp, _ := e["fp"].(string)
			off, _ := extract.Offset(e["offset"])
			st.Files[k] = fileState{fp, off}
		}
	}
	if m, ok := top["captured"].(map[string]any); ok {
		for k, v := range m {
			switch e := v.(type) {
			case map[string]any:
				n, _ := e["at"].(json.Number)
				at, _ := n.Float64()
				status, _ := e["status"].(string)
				st.Captured[k] = captured{at, status}
			case json.Number:
				at, _ := e.Float64()
				st.Captured[k] = captured{at, "uploaded"}
			}
		}
	}
	if n, ok := top["last_pass"].(json.Number); ok {
		st.LastPass, _ = n.Float64()
	}
	if m, ok := top["drifted"].(map[string]any); ok {
		for k, v := range m {
			st.Drifted[k], _ = extract.Offset(v)
		}
	}
	return st, nil
}

func (st state) save(dir string) error {
	files := map[string]any{}
	for k, v := range st.Files {
		files[k] = map[string]any{"fp": v.FP, "offset": v.Offset}
	}
	caps := map[string]any{}
	for k, v := range st.Captured {
		caps[k] = map[string]any{"at": v.At, "status": v.Status}
	}
	top := map[string]any{"version": stateVersion, "files": files, "captured": caps, "last_pass": st.LastPass}
	if len(st.Drifted) > 0 {
		// Only when a file is held, so the state stays the Python tool's shape otherwise.
		drifted := map[string]any{}
		for k, v := range st.Drifted {
			drifted[k] = v
		}
		top["drifted"] = drifted
	}
	out := dumps(top, true)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, StateName) + ".tmp"
	if err := os.WriteFile(tmp, []byte(out), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, StateName))
}

// transcript is one file a pass reads: its state key, path and reader.
type transcript struct {
	key, path   string
	fingerprint func() (string, int64, error)
	read        func(start, holdFrom int64) (claudecode.PromptRead, error)
}

func listTranscripts(o Options, scope *extract.Scope) (out []transcript, sourcePaths []string) {
	for _, root := range o.Roots {
		rels, paths := claudecode.TranscriptFiles(root)
		for i, p := range paths {
			p, rel := p, rels[i]
			out = append(out, transcript{key: claudecode.PathKey(rel), path: p,
				fingerprint: func() (string, int64, error) {
					fp, err := claudecode.FirstLineHash(p)
					if err != nil {
						return "", 0, err
					}
					st, err := os.Stat(p)
					if err != nil {
						return "", 0, err
					}
					return fp, st.Size(), nil
				},
				read: func(start, holdFrom int64) (claudecode.PromptRead, error) {
					return claudecode.PromptsIn(scope, p, start, holdFrom)
				}})
		}
	}
	if o.CodexRoot != "" {
		for _, p := range codex.RolloutFiles(o.CodexRoot) {
			p := p
			out = append(out, transcript{key: "codex:" + claudecode.PathKey(p), path: p,
				fingerprint: func() (string, int64, error) { return codex.Fingerprint(p) },
				read: func(start, _ int64) (claudecode.PromptRead, error) {
					return codexPrompts(scope, p, start)
				}})
		}
	}
	return out, nil
}

func when(ts any) (time.Time, bool) {
	t, ok, _ := aware(ts)
	return t, ok
}

// Run is one pass. It returns counts only and never raises: a failure is a ledger.error.
func Run(o Options) (counts map[string]any) {
	w := &extract.Writer{Dir: o.Dir, Stderr: o.Stderr, DryRun: o.DryRun}
	defer func() {
		if p := recover(); p != nil {
			w.Error(fmt.Sprintf("prompt_capture: unhandled %T", p))
			counts = map[string]any{"error": fmt.Sprintf("%T", p)}
		}
	}()
	return run(o, w)
}

type pass struct {
	o       Options
	w       *extract.Writer
	counts  map[string]any
	status  map[string]int
	rules   map[string]int
	tickets *ticket.Matcher // nil: the default pattern
}

func (p *pass) say(reason string) {
	p.counts["errors"] = intOf(p.counts["errors"]) + 1
	if !p.o.DryRun {
		p.w.Error("prompt_capture: " + reason)
	}
}

func intOf(v any) int { n, _ := v.(int); return n }

func run(o Options, w *extract.Writer) map[string]any {
	if refused := refusal(o); refused != "" {
		return map[string]any{"refused": refused}
	}
	st, err := loadState(o.Dir)
	if err != nil {
		w.Error(fmt.Sprintf("prompt_capture: %s %v; held: no transcript offset moved and the file is left as it is, "+
			"so no prompt is captured or skipped until it is fixed or removed (removing it starts again at the end of "+
			"every transcript)", StateName, err))
		return map[string]any{"held": StateName + " " + err.Error()}
	}
	now := float64(o.Now.UnixNano()) / 1e9
	if o.BackfillDays == nil && now-st.LastPass < PassEvery.Seconds() {
		return map[string]any{"skipped": "ran within the hour"}
	}
	scope := extract.NewScope(o.Stderr)
	c := newCollection(o, st)
	files, _ := listTranscripts(o, scope)
	for _, f := range files {
		c.file(f, scope)
	}
	upload := UploadOn() || o.DryRun
	p := &pass{o: o, w: w, counts: map[string]any{}, status: map[string]int{}, rules: map[string]int{}}
	tickets, problem := ticket.FromSettings(hook.Settings())
	if p.tickets = tickets; problem != "" && len(c.prompts) > 0 {
		p.say(problem)
	}
	events, records, relayProofs, relayOutside, withheld, problems := p.decide(c.prompts, upload, scope)
	byStatus, refused, probs := map[string]any{}, map[string]any{}, map[string]any{}
	for k, v := range p.status {
		byStatus[k] = v
	}
	for k, v := range p.rules {
		refused[k] = v
	}
	for k, v := range problems {
		probs[k] = v
	}
	p.counts["by_status"], p.counts["refused_rules"], p.counts["matcher_problems"] = byStatus, refused, probs
	p.counts["relay_proofs"], p.counts["relay_outside"], p.counts["context_withheld"] = relayProofs, relayOutside, withheld
	p.counts["prompts"], p.counts["first_pass"], p.counts["unreadable_lines"] = len(c.prompts), c.first, c.unreadable
	p.counts["no_session"], p.counts["scope_held_files"] = len(c.noSession), c.held
	if o.DryRun {
		p.counts["would_upload"] = len(records)
		return p.counts
	}
	p.counts["would_upload"] = nil
	c.reportUncaptured(w)
	keepText(o, w, append(slices.Clone(c.prompts), committedOnly(o, w, c.outside)...))
	if !p.write(events, records) {
		return p.counts
	}
	c.save(w, events, now)
	return p.counts
}

// refusal is why a backfill is refused, or "".
func refusal(o Options) string {
	if o.BackfillDays == nil {
		return ""
	}
	if *o.BackfillDays < 1 {
		return "a backfill needs 1 or more days"
	}
	if *o.BackfillDays > capturedKeepDays {
		// Past the id store's memory a backfill would send again what was uploaded or refused.
		return fmt.Sprintf("a backfill reaches back at most %d days, as far as the prompt id store remembers", capturedKeepDays)
	}
	if !o.DryRun && !UploadOn() {
		return "upload is off"
	}
	return ""
}

// collection is what one pass reads: each file's next offset, the new prompts, and what it
// could not read or scope.
type collection struct {
	o          Options
	st         state
	first      bool
	since      *time.Time
	offsets    map[string]fileState
	prompts    []claudecode.Prompt
	outside    []claudecode.Prompt // read outside every in-scope repo: the vault's, when the session commits to one
	seen       map[string]bool
	unreadable int
	held       int
	noSession  []string
	drifts     []string         // why each newly held file stopped
	drifted    map[string]int64 // every file held this pass, by the offset it is held at
}

func newCollection(o Options, st state) *collection {
	c := &collection{o: o, st: st, first: len(st.Files) == 0 && o.BackfillDays == nil,
		offsets: map[string]fileState{}, seen: map[string]bool{}, drifted: map[string]int64{}}
	if o.BackfillDays != nil {
		s := o.Now.UTC().Add(-time.Duration(*o.BackfillDays) * 24 * time.Hour)
		c.since = &s
	}
	return c
}

// file reads one transcript past its offset (from its start in a backfill).
func (c *collection) file(f transcript, scope *extract.Scope) {
	fp, size, err := f.fingerprint()
	if err != nil {
		c.unreadable++
		return
	}
	known := c.st.Files[f.key]
	start := int64(0)
	if known.FP == fp {
		start = known.Offset
	}
	if c.first {
		c.offsets[f.key] = fileState{fp, size}
		return
	}
	if c.since == nil && start >= size {
		if _, ok := c.st.Files[f.key]; ok {
			c.offsets[f.key] = known
		} else {
			c.offsets[f.key] = fileState{}
		}
		return
	}
	before := scope.TimeoutReads
	readFrom := start
	if c.since != nil {
		readFrom = 0
	}
	read, err := f.read(readFrom, start)
	if err != nil {
		c.unreadable++
		return
	}
	if scope.TimeoutReads != before {
		c.held++ // a prompt's scope is unknown: read the whole file again next pass
		return
	}
	c.unreadable += read.Bad
	for _, m := range read.NoSession {
		if _, ok := c.st.Captured[m]; !ok {
			c.noSession = append(c.noSession, m)
		}
	}
	c.offsets[f.key] = fileState{fp, max(read.Offset, start)}
	if read.Held != "" {
		if at, said := c.st.Drifted[f.key]; !said || at != read.Offset {
			c.drifts = append(c.drifts, read.Held)
		}
		c.drifted[f.key] = read.Offset
	}
	for _, pr := range read.Found {
		if c.isNew(pr) {
			c.seen[pr.ID] = true
			c.prompts = append(c.prompts, pr)
		}
	}
	for _, pr := range read.Outside {
		t, ok := when(pr.TS)
		if ok && !c.seen[pr.ID] && (c.since == nil || !t.Before(*c.since)) {
			c.seen[pr.ID] = true
			c.outside = append(c.outside, pr)
		}
	}
}

// isNew is whether a prompt is to be decided this pass: not seen, inside a backfill's
// window, and not captured before unless a backfill may send it again.
func (c *collection) isNew(pr claudecode.Prompt) bool {
	t, ok := when(pr.TS)
	if c.seen[pr.ID] || (c.since != nil && (!ok || t.Before(*c.since))) {
		return false
	}
	cp, in := c.st.Captured[pr.ID]
	return !in || (c.since != nil && resendable[cp.Status])
}

// reportUncaptured says which transcripts are held and which prompts have no session.
func (c *collection) reportUncaptured(w *extract.Writer) {
	if c.held > 0 {
		w.Error(fmt.Sprintf("prompt_capture: %d transcript(s) held for the next pass: git timed out scoping a working directory", c.held))
	}
	for _, d := range c.drifts {
		w.Error("prompt_capture: a transcript holds " + d + " — the transcript format may have changed; that file is " +
			"held before it, and its prompts from there on are NOT captured or kept until Livy reads it")
	}
	if len(c.noSession) == 0 {
		return
	}
	sort.Strings(c.noSession)
	uniq := c.noSession[:0]
	for i, id := range c.noSession {
		if i == 0 || id != c.noSession[i-1] {
			uniq = append(uniq, id)
		}
	}
	if len(uniq) > 20 {
		uniq = uniq[:20]
	}
	w.Error(fmt.Sprintf("prompt_capture: %d prompt(s) with no session, not captured: %s", len(c.noSession), strings.Join(uniq, ", ")))
}

// write writes the text records, then the prompt.captured lines, each only once across
// retries; false when either failed, so the pass is retried.
func (p *pass) write(events []map[string]any, records []*record) bool {
	w := p.w
	// Retried after a failure below, a pass must not write again what it already wrote.
	texts, eventsHere := alreadyWritten(p.o.Dir)
	var fresh []string
	for _, r := range records {
		if !texts[fmt.Sprint(r.ID)] {
			fresh = append(fresh, r.json())
		}
	}
	ok, unchecked := appendText(p.o.Dir, fresh)
	if !ok {
		w.Error("prompt_capture: the text records could not be written; this pass is retried")
		return false
	}
	if unchecked != nil {
		w.Error(fmt.Sprintf("prompt_capture: the text records' last line could not be checked for a torn write (%v); %d written without it",
			unchecked, len(fresh)))
	}
	var lines []map[string]any
	for _, e := range events {
		if !eventsHere[[2]string{e["prompt_id"].(string), e["text_status"].(string)}] {
			lines = append(lines, e)
		}
	}
	failed, invalid := w.Failed, w.Invalid
	w.Write(lines)
	if w.Failed != failed || w.Invalid != invalid {
		w.Error(fmt.Sprintf("prompt_capture: %d prompt.captured line(s) failed, %d refused; this pass is retried",
			w.Failed-failed, w.Invalid-invalid))
		return false
	}
	return true
}

// save records what was captured and where each file was read to, forgets captures older
// than the id store keeps, and saves the state.
func (c *collection) save(w *extract.Writer, events []map[string]any, now float64) {
	st := c.st
	keepAfter := now - capturedKeep.Seconds()
	for _, e := range events {
		st.Captured[e["prompt_id"].(string)] = captured{now, e["text_status"].(string)}
	}
	for _, id := range c.noSession {
		st.Captured[id] = captured{now, "error"}
	}
	maps.Copy(st.Files, c.offsets)
	for k := range c.offsets {
		delete(st.Drifted, k) // read again this pass: held now only if c.drifted says so
	}
	maps.Copy(st.Drifted, c.drifted)
	st.LastPass = now
	for k, v := range st.Captured {
		if v.At < keepAfter {
			delete(st.Captured, k)
		}
	}
	if err := st.save(c.o.Dir); err != nil {
		w.Error(fmt.Sprintf("prompt_capture: could not save state (%T); this pass is retried", err))
	}
}

func event(pr claudecode.Prompt, status string, proof *Proof, d *scrub.Denylist, tickets *ticket.Matcher) map[string]any {
	ts := extract.UTCSeconds(pr.TS)
	if ts == "" {
		ts = clock.Seconds(clock.Now())
	}
	branch := pr.Branch
	if branch == "" {
		branch = "unknown"
	}
	e := map[string]any{"schema_version": int64(extract.SchemaVersion), "ts": ts, "event": "prompt.captured",
		"repo": pr.Repo, "branch": branch, "session_id": pr.SessionID, "prompt_id": pr.ID, "text_status": status,
		"ticket_key": tickets.Key(pr.Branch)}
	if d != nil {
		e["denylist_version"] = int64(d.Version)
	}
	e["relay_proof"], e["relay_proof_share"], e["relay_source"], e["relay_lag_minutes"] = nil, nil, nil, nil
	if proof != nil {
		e["relay_proof"], e["relay_proof_share"] = proof.Proof, proof.Share
		if proof.Source != nil {
			e["relay_source"] = *proof.Source
		}
		if proof.LagMins != nil {
			e["relay_lag_minutes"] = *proof.LagMins
		}
	}
	return e
}

// decide is the events, text records and counts for new prompts. It writes only its
// ledger.errors, and in a dry run not those.
func (p *pass) decide(prompts []claudecode.Prompt, upload bool, scope *extract.Scope) (events []map[string]any, records []*record,
	relayProofs, relayOutside, withheld int, problems map[string]int) {
	problems = map[string]int{}
	var names *scrub.Denylist
	why := ""
	if upload {
		names, why = LoadDenylist()
	}
	tickets := p.tickets
	if tickets == nil {
		tickets = ticket.Default()
	}
	add := func(pr claudecode.Prompt, status string, proof *Proof) {
		p.status[status]++
		events = append(events, event(pr, status, proof, names, tickets))
	}
	if !upload || names == nil {
		for _, pr := range prompts {
			status := "upload_off"
			if upload {
				status = "no_denylist"
			}
			add(pr, status, nil)
			if upload {
				p.say(fmt.Sprintf("%s not uploaded: no usable denylist (%s)", pr.ID, why))
			}
		}
		return
	}
	times := make([]*time.Time, len(prompts))
	oldest := p.o.Now
	haveOldest := false
	for i, pr := range prompts {
		if t, ok := when(pr.TS); ok {
			times[i] = &t
			if !haveOldest || t.Before(oldest) {
				oldest, haveOldest = t, true
			}
		}
	}
	built := make([]*record, len(prompts))
	wanted := map[string]bool{}
	for i, pr := range prompts {
		texts := []string{pr.Text, pr.AgentBefore}
		for _, e := range pr.Exchanges {
			texts = append(texts, e.Person, e.Agent)
		}
		if times[i] != nil {
			built[i] = textRecord(pr, names)
			texts = append(texts, built[i].contextTexts()...)
		}
		for _, t := range texts {
			for s := range shingles(t) {
				wanted[s] = true
			}
		}
	}
	sources := newSources(wanted)
	if len(wanted) > 0 {
		cutoff := oldest.Add(-window)
		for _, f := range p.sourceFiles(cutoff) {
			if f.codex {
				sources.addCodexFile(f.path)
			} else {
				sources.addFile(f.path)
			}
		}
	}
	// A source that could not be read may be the outside session a prompt came from. A git
	// lookup that timed out is not one of these: it already resolves to outside.
	repoOf := sourceRepo(scope, sources.Problems)
	unjudged := 0
	for k, v := range sources.Problems {
		if k != "source_scope_timeout" {
			unjudged += v
		}
	}
	for i, pr := range prompts {
		if times[i] == nil || built[i] == nil {
			add(pr, "error", nil)
			p.say(pr.ID + " has no readable time")
			continue
		}
		if unjudged > 0 {
			add(pr, "error", nil)
			continue
		}
		proof := sources.match(pr.Text, pr.SessionID, *times[i], repoOf)
		if proof.Proof == "yes" {
			relayProofs++
		}
		if proof.Source != nil && *proof.Source == Outside {
			relayOutside++
			add(pr, "outside_relay", &proof)
			continue
		}
		rec := built[i]
		withheld += rec.withholdOutside(pr, func(text string) bool {
			m := sources.match(text, pr.SessionID, *times[i], repoOf)
			return m.Source != nil && *m.Source == Outside
		})
		if hits := scrub.AuditValue(rec.fields(), names); len(hits) > 0 {
			for _, h := range hits {
				p.rules[h]++
			}
			add(pr, "refused_audit", &proof)
			p.say(fmt.Sprintf("%s refused by the audit: %s", pr.ID, strings.Join(hits, ", ")))
			continue
		}
		records = append(records, rec)
		add(pr, "uploaded", &proof)
	}
	for k, v := range sources.Problems {
		problems[k] = v
	}
	if len(problems) > 0 {
		var keys []string
		for k := range problems {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %d", k, problems[k]))
		}
		msg := "transcript records the relay reader could not read: " + strings.Join(parts, ", ")
		if unjudged > 0 {
			msg += fmt.Sprintf("; %d prompt(s) not judged, no text written", len(prompts))
		}
		p.say(msg)
	}
	return
}

type sourceFile struct {
	path  string
	codex bool
}

// sourceFiles are the transcripts modified since cutoff: where a relay's source may be.
func (p *pass) sourceFiles(cutoff time.Time) []sourceFile {
	var out []sourceFile
	add := func(path string, codexFile bool) {
		if st, err := os.Stat(path); err == nil && !st.ModTime().Before(cutoff) {
			out = append(out, sourceFile{path, codexFile})
		}
	}
	for _, root := range p.o.Roots {
		_, paths := claudecode.TranscriptFiles(root)
		for _, path := range paths {
			add(path, false)
		}
	}
	if p.o.CodexRoot != "" {
		for _, path := range codex.RolloutFiles(p.o.CodexRoot) {
			add(path, true)
		}
	}
	return out
}

// alreadyWritten is the prompt ids with a text record still on this machine, and the
// (prompt id, text_status) pairs of prompt.captured events still here: what a retried pass
// must not write twice. What already shipped is in state.
func alreadyWritten(dir string) (texts map[string]bool, events map[[2]string]bool) {
	texts, events = map[string]bool{}, map[[2]string]bool{}
	textPaths, _ := filepath.Glob(filepath.Join(dir, SegmentsDir, "*", "*.jsonl"))
	flat, _ := filepath.Glob(filepath.Join(dir, SegmentsDir, "*.jsonl"))
	for _, path := range append(append(textPaths, flat...), filepath.Join(dir, TextFile)) {
		for _, row := range jsonLines(path) {
			texts[extract.PyStr(row["prompt_id"])] = true
		}
	}
	ledgerPaths, _ := filepath.Glob(filepath.Join(dir, "segments", "*", "*.jsonl"))
	for _, path := range append(ledgerPaths, filepath.Join(dir, ledger.FileName)) {
		for _, row := range jsonLines(path) {
			if row["event"] == "prompt.captured" {
				events[[2]string{extract.PyStr(row["prompt_id"]), extract.PyStr(row["text_status"])}] = true
			}
		}
	}
	return texts, events
}

func jsonLines(path string) []map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rows []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		if v, err := extract.Decode([]byte(line)); err == nil {
			if m, ok := v.(map[string]any); ok {
				rows = append(rows, m)
			}
		}
	}
	return rows
}

// appendText appends lines to the text records under their lock; false when they were not
// written. unchecked is why the file's last line could not be checked for a torn write: the
// lines were then written without it.
func appendText(dir string, lines []string) (written bool, unchecked error) {
	if len(lines) == 0 {
		return true, nil
	}
	release, err := ledger.Lock(filepath.Join(dir, TextLock), 10*time.Second)
	if err != nil {
		return false, nil
	}
	defer release()
	// A write that fails partway is cut back to where it started, as ledger.Append does, so the next
	// record never lands glued to half of this one; a write killed partway cannot be cut back, so
	// the next append ends its fragment first. Safe because every writer holds TextLock.
	t, err := ledger.OpenTail(filepath.Join(dir, TextFile))
	if err != nil {
		return false, nil
	}
	text := []byte(strings.Join(lines, "\n") + "\n")
	if _, err := t.File.Write(t.Lead(text)); err != nil {
		t.CutBack()
		_ = t.File.Close() // the failed write is what is reported
		return false, nil
	}
	return t.File.Close() == nil, t.Unchecked
}
