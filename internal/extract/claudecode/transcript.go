package claudecode

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/0xbew/livy/internal/extract"
)

// nameAt is a tool call's name and the index of the line that started it (-1: carried in).
type nameAt struct {
	name string
	at   int
}

type linkAt struct {
	index int
	link  map[string]any
}

type interAt struct {
	index int
	it    interaction
}

// scanState is where the human was, and whether the last call failed, before a line.
type scanState struct {
	ctx        ctxT
	afterError bool
}

// pendingCall is a call with no result before the cut, carried to the next pass.
type pendingCall struct {
	used     int
	id, name string
}

// transcriptScan is one readTranscript pass: the offset each family starts at, and the
// context tracked across every line read.
type transcriptScan struct {
	rd                                                 *reader
	res                                                *parsed
	start, interactionStart, detailStart, outcomeStart int64
	offset                                             int64
	offsets                                            []int64

	// outcomes
	carriedNames    map[string]string
	carriedWhere    map[string][2]string
	elsewhere       map[string]bool
	names           map[string]nameAt
	resolved        map[string]int
	callAt          map[string]callWhere
	outcomeItems    []*outcomeItem
	enteredOutcomes bool
	// skillUses are the calls that used a skill, by id; skillDirs the folder Claude Code said a
	// Skill call's skill is in, by the call's id.
	skillUses map[string]skillUse
	skillDirs map[string]string

	// usage
	spans                   map[string][2]int
	best                    map[string]*kept
	bestOrder               []string
	links                   []linkAt
	assistantIdx, usableIdx []int

	// interactions
	inters     []interAt
	ctx        ctxT
	afterError bool
	before     []scanState

	// details
	carriedPrompt              string
	prompt                     string
	promptSeen, enteredDetails bool
	details                    map[string]*detail
	detailOrder                []string
	promptBefore               []string
}

func (rd *reader) newScan(start, interactionStart, detailStart, outcomeStart int64) *transcriptScan {
	begin := min(start, interactionStart, detailStart, outcomeStart)
	return &transcriptScan{
		rd:               rd,
		res:              &parsed{nextOffset: begin},
		start:            start,
		interactionStart: interactionStart,
		detailStart:      detailStart,
		outcomeStart:     outcomeStart,
		offset:           begin,
		carriedNames:     map[string]string{},
		carriedWhere:     map[string][2]string{},
		elsewhere:        map[string]bool{},
		names:            map[string]nameAt{},
		resolved:         map[string]int{},
		callAt:           map[string]callWhere{},
		skillUses:        map[string]skillUse{},
		skillDirs:        map[string]string{},
		spans:            map[string][2]int{},
		best:             map[string]*kept{},
		details:          map[string]*detail{},
	}
}

// loadCarries takes what the last pass left for each family that starts past the file's head.
func (s *transcriptScan) loadCarries(carry, detailCarry, outcomeCarryIn any) {
	if s.outcomeStart > 0 {
		s.carriedNames, s.carriedWhere, s.elsewhere = s.rd.carried(outcomeCarryIn)
	}
	for k, v := range s.carriedNames {
		s.names[k] = nameAt{v, -1}
	}
	if s.outcomeStart > 0 {
		s.skillUses = carriedSkills(outcomeCarryIn, s.carriedNames)
		s.res.outcomes = waitingSkills(outcomeCarryIn)
	}
	if s.interactionStart > 0 {
		s.ctx, s.afterError = s.rd.interactionCarry(carry)
	}
	if s.detailStart > 0 {
		s.carriedPrompt = carriedPromptOf(detailCarry)
	}
}

// interactionCarry is the context and after-error flag the last pass left, or the zero
// context when any part of it is not in the shape this reader writes.
func (rd *reader) interactionCarry(carry any) (ctxT, bool) {
	c, ok := carry.(map[string]any)
	if !ok {
		return ctxT{}, false
	}
	known, ok1 := c["known"].(bool)
	ae, ok2 := c["after_error"].(bool)
	repo, repoOK := rd.carriedRepo(c["repo"])
	branch, branchOK := carriedBranch(c["branch"])
	if !ok1 || !ok2 || !repoOK || !branchOK {
		return ctxT{}, false
	}
	if repo == "" {
		branch = ""
	}
	return ctxT{known, repo, branch}, ae
}

func (rd *reader) carriedRepo(v any) (string, bool) {
	switch r := v.(type) {
	case nil:
		return "", true
	case string:
		return r, r == extract.Unknown || (isSlug(r) && rd.scope.Git.InScope("https://github.com/"+r, rd.scope.Entries))
	}
	return "", false
}

func carriedBranch(v any) (string, bool) {
	switch b := v.(type) {
	case nil:
		return "", true
	case string:
		return b, true
	}
	return "", false
}

func carriedPromptOf(detailCarry any) string {
	c, ok := detailCarry.(map[string]any)
	if !ok {
		return ""
	}
	if p, ok := c["prompt"].(string); ok && promptLinkRe.MatchString(p) {
		return p
	}
	return ""
}

// readLines reads complete lines from the scan's offset until EOF, a line still being
// written, or a record the scan holds the file before.
func (s *transcriptScan) readLines(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only: nothing to flush
	if _, err := f.Seek(s.offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek transcript: %w", err)
	}
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		raw, err := br.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return fmt.Errorf("read transcript: %w", err)
		}
		if len(raw) == 0 || raw[len(raw)-1] != '\n' {
			return nil // a line still being written
		}
		if !s.line(raw) {
			return nil
		}
	}
}

// line takes one complete line; false when the file is held before it.
func (s *transcriptScan) line(raw []byte) bool {
	index := len(s.offsets)
	lineOffset := s.offset
	s.offsets = append(s.offsets, lineOffset)
	inDetails := lineOffset >= s.detailStart
	if inDetails && !s.enteredDetails {
		s.enteredDetails = true
		if !s.promptSeen {
			s.prompt = s.carriedPrompt
		}
	}
	s.before = append(s.before, scanState{s.ctx, s.afterError})
	s.promptBefore = append(s.promptBefore, s.prompt)
	s.offset += int64(len(raw))
	inUsage := lineOffset >= s.start
	v, derr := extract.Decode(raw)
	rec, isObj := v.(map[string]any)
	if derr != nil || !isObj {
		if inUsage {
			s.res.badLines++
		}
		return true
	}
	kind, _ := rec["type"].(string)
	if what := s.unrecognisedShape(rec, kind, lineOffset); what != "" {
		s.hold(index, rec, what, lineOffset)
		return false
	}
	if !s.noteContext(rec, kind, index, lineOffset) {
		return false
	}
	s.noteCalls(rec, kind, index, lineOffset)
	if kind == "assistant" && inDetails {
		s.rd.collectDetail(rec, s.details, &s.detailOrder, s.prompt, lineOffset)
	}
	if kind == "pr-link" {
		if inUsage {
			s.noteLink(rec, index)
		}
		return true
	}
	s.noteAssistant(rec, kind, index, inUsage, inDetails)
	return true
}

// unrecognisedShape says why a record the scan must hold before is not recognised, or "".
func (s *transcriptScan) unrecognisedShape(rec map[string]any, kind string, lineOffset int64) string {
	inUsage := lineOffset >= s.start
	if !knownTypes[kind] && inUsage {
		if extract.IsName(kind) {
			return "a record of the unknown type " + kind
		}
		return "a record of an unknown type"
	}
	if kind == "assistant" && inUsage && !usable(rec) {
		return "an assistant record with no usage this extractor recognises"
	}
	if kind == "user" && lineOffset >= s.interactionStart && !userShapeKnown(rec) {
		return unknownUserShape
	}
	return ""
}

// hold records the drift at line index and rewinds the scan to before that line.
func (s *transcriptScan) hold(index int, rec map[string]any, what string, lineOffset int64) {
	s.res.drift = &drift{index, recordVersion(rec), what}
	s.offsets = s.offsets[:index]
	s.before, s.promptBefore = s.before[:index], s.promptBefore[:index]
	s.offset = lineOffset
}

// noteContext moves the human's context and the prompt in effect past this line, and keeps
// its interactions; false when the line is a user message this reader cannot read.
func (s *transcriptScan) noteContext(rec map[string]any, kind string, index int, lineOffset int64) bool {
	own := s.rd.contextFrom(rec)
	sidechain := rec["isSidechain"] == true
	ownMain := own != nil && !sidechain
	if ownMain {
		s.ctx = *own
	}
	found, ae, unrecognised := s.rd.interactionsOf(rec, s.ctx, s.afterError, lineOffset)
	s.afterError = ae
	if s.ctx.Known && s.ctx.Repo == "" {
		s.afterError = false
	}
	inInteractions := lineOffset >= s.interactionStart
	if unrecognised && kind == "user" && !sidechain {
		if inInteractions {
			st := s.before[index]
			s.ctx, s.afterError, s.prompt = st.ctx, st.afterError, s.promptBefore[index]
			s.hold(index, rec, "a user message of a shape this extractor does not recognise", lineOffset)
			return false
		}
		s.prompt, s.promptSeen = "", true
	}
	s.notePrompts(found, ownMain && own.Repo != "")
	if inInteractions {
		for _, it := range found {
			s.inters = append(s.inters, interAt{index, it})
		}
	}
	return true
}

// notePrompts makes the last prompt found the one in effect, kept only inside a repo.
func (s *transcriptScan) notePrompts(found []interaction, inRepo bool) {
	for _, it := range found {
		if it.Kind != "prompt" {
			continue
		}
		s.promptSeen = true
		s.prompt = ""
		if inRepo {
			s.prompt = it.ID
		}
	}
}

// noteCalls names the calls every line read starts or resolves; only lines past the
// outcome offset emit.
func (s *transcriptScan) noteCalls(rec map[string]any, kind string, index int, lineOffset int64) {
	inOutcomes := lineOffset >= s.outcomeStart
	s.enteredOutcomes = s.enteredOutcomes || inOutcomes
	msg, _ := rec["message"].(map[string]any)
	content, isList := msg["content"].([]any)
	if !isList {
		return
	}
	switch kind {
	case "assistant":
		s.noteToolUses(rec, content, index)
	case "user":
		s.noteToolResults(rec, content, index, inOutcomes)
		if id, dir := skillDirOf(rec, content); id != "" {
			s.skillDirs[id] = dir
		}
	}
}

func (s *transcriptScan) noteToolUses(rec map[string]any, content []any, index int) {
	for _, b := range content {
		block, ok := b.(map[string]any)
		if !ok || block["type"] != "tool_use" {
			continue
		}
		id, ok1 := block["id"].(string)
		name, ok2 := block["name"].(string)
		if !ok1 || !toolIDRe.MatchString(id) || !ok2 || !extract.IsName(name) {
			continue
		}
		s.names[id] = nameAt{name, index}
		cwd, ok := cwdOf(rec)
		if u, used := skillUseOf(name, block["input"], cwd); used {
			s.skillUses[id] = u
		}
		if !ok {
			continue
		}
		fields := map[string]any{"cwd": cwd}
		if g, ok := rec["gitBranch"]; ok {
			fields["gitBranch"] = g
		}
		s.callAt[id] = callWhere{fields, extract.NamedBy(name, block["input"], cwd)}
	}
}

func (s *transcriptScan) noteToolResults(rec map[string]any, content []any, index int, inOutcomes bool) {
	where := map[string]any{}
	for _, k := range []string{"sessionId", "gitBranch", "cwd", "timestamp", "version", "isSidechain", "agentId"} {
		if v, ok := rec[k]; ok {
			where[k] = v
		}
	}
	for _, b := range content {
		block, ok := b.(map[string]any)
		if !ok || block["type"] != "tool_result" {
			continue
		}
		id, ok := block["tool_use_id"].(string)
		if !ok || !toolIDRe.MatchString(id) {
			if inOutcomes {
				s.outcomeItems = append(s.outcomeItems, &outcomeItem{index: index, where: where, skip: "bad_id"})
			}
			continue
		}
		if _, seen := s.resolved[id]; !seen {
			s.resolved[id] = index
		}
		if inOutcomes {
			s.outcomeItems = append(s.outcomeItems, resultItem(block, index, where, id))
		}
	}
}

func resultItem(block map[string]any, index int, where map[string]any, id string) *outcomeItem {
	as := map[string]decided{}
	for _, t := range specialTools {
		as[t] = classify(block, t)
	}
	return &outcomeItem{index: index, where: where, toolUseID: id, asSpecial: as, asOther: classify(block, ""),
		isError: block["is_error"] == true}
}

func (s *transcriptScan) noteLink(rec map[string]any, index int) {
	link := map[string]any{}
	for _, k := range []string{"sessionId", "prNumber", "prRepository", "timestamp"} {
		link[k] = rec[k]
	}
	s.links = append(s.links, linkAt{index, link})
}

// noteAssistant widens the message's span and keeps its usage when past the usage offset.
func (s *transcriptScan) noteAssistant(rec map[string]any, kind string, index int, inUsage, inDetails bool) {
	if kind != "assistant" || (!inUsage && !inDetails) {
		return
	}
	msg, _ := rec["message"].(map[string]any)
	if id, ok := msg["id"].(string); ok {
		sp, seen := s.spans[id]
		if !seen {
			sp = [2]int{index, index}
		}
		sp[1] = index
		s.spans[id] = sp
	}
	if !inUsage {
		return
	}
	s.assistantIdx = append(s.assistantIdx, index)
	if !usable(rec) {
		return // held above when it is past the usage offset; never reaches keep
	}
	s.usableIdx = append(s.usableIdx, index)
	s.rd.keep(s.best, &s.bestOrder, rec)
}

// finish keeps what lies before the cut and writes each family's carry at it.
func (s *transcriptScan) finish(settled bool) {
	end := len(s.offsets)
	c := end
	if !settled || s.res.drift != nil {
		c = cut(s.spans, end)
	}
	if c < end {
		s.res.nextOffset = s.offsets[c]
	} else {
		s.res.nextOffset = s.offset
	}
	s.res.assistant = countBefore(s.assistantIdx, c)
	s.res.usable = countBefore(s.usableIdx, c)
	s.keepMessages(c)
	s.keepLinksAndInteractions(c)
	s.res.carry = s.interactionCarryAt(c, end)
	s.keepDetails(c)
	s.res.detailCarry = s.detailCarryAt(c, end)
	s.keepOutcomes(c)
	if s.enteredOutcomes {
		s.res.outcomeCarry = s.outcomeCarryAt(c)
	} else {
		s.res.outcomeCarry = s.carriedOutcomeCarry()
	}
}

func countBefore(indexes []int, c int) int {
	n := 0
	for _, i := range indexes {
		if i < c {
			n++
		}
	}
	return n
}

func (s *transcriptScan) keepMessages(c int) {
	for _, id := range s.bestOrder {
		k := s.best[id]
		if s.spans[id][1] >= c {
			continue
		}
		msg, _ := k.rec["message"].(map[string]any)
		if msg["model"] == syntheticModel {
			s.res.synthetic++
			continue
		}
		s.res.messages = append(s.res.messages, *k)
	}
}

func (s *transcriptScan) keepLinksAndInteractions(c int) {
	for _, l := range s.links {
		if l.index < c {
			s.res.prLinks = append(s.res.prLinks, l.link)
		}
	}
	for _, it := range s.inters {
		if it.index < c {
			s.res.interactions = append(s.res.interactions, it.it)
		}
	}
}

func (s *transcriptScan) interactionCarryAt(c, end int) map[string]any {
	atCut := scanState{s.ctx, s.afterError}
	if c < end {
		atCut = s.before[c]
	}
	return map[string]any{"known": atCut.ctx.Known, "repo": nilIfEmpty(atCut.ctx.Repo),
		"branch": nilIfEmpty(atCut.ctx.Branch), "after_error": atCut.afterError}
}

func (s *transcriptScan) keepDetails(c int) {
	for _, mid := range s.detailOrder {
		d := s.details[mid]
		if sp, ok := s.spans[mid]; !ok || sp[1] >= c {
			continue
		}
		if d.Synthetic && d.Error == nil {
			continue
		}
		if d.StopReason == "tool_use" && len(d.Tools) == 0 {
			d.Incomplete = true
		}
		s.res.details = append(s.res.details, d)
	}
}

func (s *transcriptScan) detailCarryAt(c, end int) map[string]any {
	if !s.enteredDetails {
		return map[string]any{"prompt": nilIfEmpty(s.carriedPrompt)}
	}
	p := s.prompt
	if c < end {
		p = s.promptBefore[c]
	}
	return map[string]any{"prompt": nilIfEmpty(p)}
}

func (s *transcriptScan) nameOf(id string) (string, bool) {
	n, ok := s.names[id]
	return n.name, ok
}

func (s *transcriptScan) keepOutcomes(c int) {
	for _, it := range s.outcomeItems {
		if it.index >= c {
			continue
		}
		fin := s.rd.finishOutcome(it, s.nameOf, s.callAt, s.carriedWhere, s.elsewhere)
		if fin == nil {
			continue
		}
		if fin.skill = s.skillOf(fin.toolUseID); !fin.quiet || fin.skill != nil {
			s.res.outcomes = append(s.res.outcomes, fin)
		}
	}
}

// outcomeCarryAt is the calls still waiting for a result at the cut (newest first, capped),
// where each started, and the hashes of calls out of scope.
func (s *transcriptScan) outcomeCarryAt(c int) map[string]any {
	started := map[string][]any{}
	out := map[string]bool{}
	for h := range s.elsewhere {
		out[h] = true
	}
	for id, at := range s.resolved {
		if at < c {
			delete(out, idHash(id))
		}
	}
	var pending []pendingCall
	for id, na := range s.names {
		if na.at >= c {
			continue
		}
		if at, ok := s.resolved[id]; ok && at < c {
			continue
		}
		place, in := s.startOf(id)
		if !in {
			out[idHash(id)] = true
			continue
		}
		if place != nil {
			started[id] = place
		}
		pending = append(pending, pendingCall{na.at, id, na.name})
	}
	pending = newestPending(pending)
	pm, sm, sk := map[string]any{}, map[string]any{}, map[string]any{}
	for _, p := range pending {
		pm[p.id] = p.name
		if st, ok := started[p.id]; ok {
			sm[p.id] = st
		}
		if u := s.skillOf(p.id); u != nil {
			sk[p.id] = []any{u.trigger, u.name, u.dir}
		}
	}
	hashes := sortedKeys(out)
	if len(hashes) > outcomeCarryCap {
		hashes = hashes[:outcomeCarryCap]
	}
	return map[string]any{"pending": pm, "started": sm, "elsewhere": stringsAny(hashes), "skills": sk}
}

// startOf is where a pending call started ([repo, branch], nil when unknown), and false when
// it started out of scope.
func (s *transcriptScan) startOf(id string) ([]any, bool) {
	if cw, ok := s.callAt[id]; ok {
		cwd, _ := cwdOf(cw.fields)
		placed, in := s.rd.scope.Place(cwd, true, []*extract.Named{cw.named})
		if !in {
			return nil, false
		}
		branch := any(nil)
		if placed.KeepBranch {
			if b, ok := extract.Str(cw.fields["gitBranch"]); ok {
				branch = b
			}
		}
		return []any{placed.Repo, branch}, true
	}
	if w, ok := s.carriedWhere[id]; ok {
		return []any{w[0], nilIfEmpty(w[1])}, true
	}
	return nil, true
}

// newestPending sorts newest first (Python sorts the (index, id, name) tuples in reverse)
// and caps the list.
func newestPending(pending []pendingCall) []pendingCall {
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].used != pending[j].used {
			return pending[i].used > pending[j].used
		}
		if pending[i].id != pending[j].id {
			return pending[i].id > pending[j].id
		}
		return pending[i].name > pending[j].name
	})
	if len(pending) > outcomeCarryCap {
		pending = pending[:outcomeCarryCap]
	}
	return pending
}

// carriedOutcomeCarry passes the last pass's carry on unchanged when no line reached the
// outcome offset.
func (s *transcriptScan) carriedOutcomeCarry() map[string]any {
	pm, sm, sk := map[string]any{}, map[string]any{}, map[string]any{}
	for k, v := range s.carriedNames {
		pm[k] = v
		if u := s.skillOf(k); u != nil {
			sk[k] = []any{u.trigger, u.name, u.dir}
		}
	}
	for k, v := range s.carriedWhere {
		sm[k] = []any{v[0], nilIfEmpty(v[1])}
	}
	return map[string]any{"pending": pm, "started": sm, "elsewhere": stringsAny(sortedKeys(s.elsewhere)), "skills": sk}
}

func sortedKeys(m map[string]bool) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
