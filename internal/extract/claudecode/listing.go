package claudecode

import (
	"regexp"

	"github.com/0xbew/livy/internal/extract"
	"github.com/0xbew/livy/internal/skill"
)

// skills.listed from the transcript: the skills Claude Code listed to the model as available, at
// a session's start (initial) and when more became available later (an update, which lists only
// the new ones). Names and digests only: the listing's text holds every skill's description, which
// is never read. A name goes out as a Skill call's does (lineName); one that is not a skill name is
// left out and counted.
//
// Where the line goes: the record's own folder, as a usage.api_call is, else the repo of the same
// agent's next call, as a skill use with nothing else to place it by (nextCall): a session Claude
// Code started outside every in-scope repo that then works in one. One line per listing, in the
// repo of that first call; a pull request joins listings by session, so one in a second repo the
// session worked in still counts it. With no call in scope after it, the listing leaves no trace.

const (
	attachmentListing = "skill_listing"
	eventSkillsListed = "skills.listed"
	// listingOffsets is where each transcript's listings are read from. It replaced
	// oldListingOffsets, under which a Livy that placed listings by their own folder alone read
	// every transcript to its end, so those transcripts are read again for their listings.
	listingOffsets    = "listings_v2"
	oldListingOffsets = "listings"
	// listingCarry holds the listings a pass in a file that may still grow could not place yet.
	listingCarry = "listing_carry"
)

var (
	listingIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	digestRe    = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
)

// listingAt is a skill listing read at line index.
type listingAt struct {
	index int
	l     *listing
}

// listing is one skill_listing record: where it was written, and its skills by line name with
// their digests (no digest from a Claude Code that wrote none).
type listing struct {
	id       string
	where    map[string]any // sessionId, gitBranch, cwd, timestamp, version, isSidechain, agentId
	initial  bool
	count    *int64
	skills   []any
	badNames int
	// problem is why the record's names could not be read ("" when they could); unnamed is a
	// listing from a Claude Code that wrote no names at all.
	problem string
	unnamed bool
}

// noteListing keeps the skill listing a record holds, if any.
func (s *transcriptScan) noteListing(rec map[string]any, index int) {
	a, _ := rec["attachment"].(map[string]any)
	if a["type"] != attachmentListing {
		return
	}
	s.listings = append(s.listings, listingAt{index, listingOf(rec, a)})
}

// listingOf reads a skill_listing record's names and digests, never its content.
func listingOf(rec, a map[string]any) *listing {
	l := &listing{where: map[string]any{}}
	for _, k := range listingWhere {
		if v, ok := rec[k]; ok {
			l.where[k] = v
		}
	}
	l.id, _ = rec["uuid"].(string)
	l.initial, _ = a["isInitial"].(bool)
	if n, ok := extract.Int(a["skillCount"]); ok {
		l.count = &n
	}
	rawNames, hasNames := a["names"]
	if !hasNames {
		l.unnamed = true
		return l
	}
	names, ok := rawNames.([]any)
	rawDigests, hasDigests := a["digests"]
	digests, dok := rawDigests.([]any)
	switch {
	case !ok:
		l.problem = "names is not a list"
	case hasDigests && (!dok || len(digests) != len(names)):
		l.problem = "digests is not a list as long as names"
	case !listingIDRe.MatchString(l.id):
		l.problem = "no usable uuid"
	}
	if l.problem != "" {
		return l
	}
	for i, v := range names {
		l.addSkill(v, digests, i)
	}
	return l
}

// addSkill adds the i-th listed name with its digest, or counts a name that is not a skill name.
func (l *listing) addSkill(v any, digests []any, i int) {
	raw, _ := v.(string)
	n, ok := skill.ParseName(raw)
	if !ok {
		l.badNames++
		return
	}
	item := map[string]any{"skill": lineName(n)}
	if i < len(digests) {
		if d, _ := digests[i].(string); digestRe.MatchString(d) {
			item["digest"] = d
		}
	}
	l.skills = append(l.skills, item)
}

// keepListings keeps the listings before the cut.
func (s *transcriptScan) keepListings(c int) {
	for _, la := range s.listings {
		if la.index < c {
			s.res.listings = append(s.res.listings, la.l)
		}
	}
}

// listingLines adds a skills.listed line for each listing in p not already written: placed by its
// own folder, else by the same agent's next call in scope. In a file that may still grow, a listing
// with neither waits in the listing carry for that call; in a settled one it was out of scope.
func (ps *extractPass) listingLines(p *parsed, fl *fileLines, settled bool) {
	// The calls read before the usage offset are searched with this pass's, so a re-read takes the
	// agent's first call after the listing, not the first of this pass.
	var calls []map[string]any
	callsOnce := func() []map[string]any {
		if calls == nil {
			calls = append(ps.callsAt(p.earlier), fl.lines...)
		}
		return calls
	}
	p.listingCarry = []any{}
	for _, l := range p.listings {
		switch {
		case l.unnamed:
			ps.stats["listings_unnamed"]++
			continue
		case l.problem != "":
			ps.stats["listings_unreadable"]++
			continue
		case ps.seenListings[l.id]:
			continue
		}
		ts := extract.UTCSeconds(l.where["timestamp"])
		if ts == "" {
			fl.markSeen(ps.seenListings, l.id)
			ps.stats["listing_unparseable_ts"]++
			continue
		}
		repo, branch, in := ps.listingPlace(l, ts, callsOnce)
		switch {
		case !in && !settled && len(p.listingCarry) < outcomeCarryCap:
			p.listingCarry = append(p.listingCarry, l.carried())
		case !in:
			fl.markSeen(ps.seenListings, l.id)
			ps.stats["listings_out_of_scope"]++
		default:
			fl.markSeen(ps.seenListings, l.id)
			fl.lines = append(fl.lines, listingLine(l, repo, branch, ts))
			ps.stats["skill_listings"]++
			ps.stats["listing_bad_names"] += l.badNames
		}
	}
}

// listingPlace is the repo and branch a listing goes to: its own folder's, else those of the same
// agent's next call among calls(); false when neither is in scope.
func (ps *extractPass) listingPlace(l *listing, ts string, calls func() []map[string]any) (repo, branch string, in bool) {
	cwd, haveCwd := cwdOf(l.where)
	if placed, ok := ps.scope.Place(cwd, haveCwd, nil); ok {
		return placed.Repo, branchOf(l.where, placed), true
	}
	repo, branch = nextCall(calls(), l.where, ts)
	return repo, branch, repo != ""
}

// callsAt is the usage line each of calls would have, for the ones in scope; never written.
func (ps *extractPass) callsAt(calls []kept) []map[string]any {
	var out []map[string]any
	for _, k := range calls {
		cwd, haveCwd := cwdOf(k.rec)
		placed, in := ps.scope.Place(cwd, haveCwd, k.named)
		ts := extract.UTCSeconds(k.rec["timestamp"])
		if in && ts != "" {
			out = append(out, usageLine(k.rec, placed, ts))
		}
	}
	return out
}

// listingLine is a listing's skills.listed line.
func listingLine(l *listing, repo, branch, ts string) map[string]any {
	line := base(eventSkillsListed, repo, ts)
	line["branch"] = branch
	line["listing_id"] = l.id
	line["listing_initial"] = l.initial
	skills := l.skills
	if skills == nil {
		skills = []any{}
	}
	line["listed_skills"] = skills
	if l.count != nil {
		line["skill_count"] = *l.count
	}
	setStr(line, "session_id", l.where["sessionId"])
	setStr(line, "agent_id", l.where["agentId"])
	setStr(line, "cc_version", l.where["version"])
	if b, ok := l.where["isSidechain"].(bool); ok {
		line["is_sidechain"] = b
	}
	return line
}

// listingWhere are the record fields a listing keeps, and carries while it waits.
var listingWhere = []string{"sessionId", "gitBranch", "cwd", "timestamp", "version", "isSidechain", "agentId"}

// carried is a waiting listing as the listing carry holds it.
func (l *listing) carried() map[string]any {
	skills := l.skills
	if skills == nil {
		skills = []any{}
	}
	c := map[string]any{"id": l.id, "initial": l.initial, "skills": skills, "bad_names": int64(l.badNames),
		"where": l.where}
	if l.count != nil {
		c["count"] = *l.count
	}
	return c
}

// waitingListings are the listings the last pass could not place yet, in exactly the shape
// carried writes them: they come from a state file and go into lines.
func waitingListings(carry any) []*listing {
	list, _ := carry.([]any)
	var out []*listing
	for _, v := range list {
		if l, ok := carriedListing(v); ok && len(out) < outcomeCarryCap {
			out = append(out, l)
		}
	}
	return out
}

func carriedListing(v any) (*listing, bool) {
	c, _ := v.(map[string]any)
	l := &listing{where: map[string]any{}}
	var iok bool
	l.id, _ = c["id"].(string)
	l.initial, iok = c["initial"].(bool)
	bad, bok := extract.Int(c["bad_names"])
	skills, sok := c["skills"].([]any)
	where, wok := c["where"].(map[string]any)
	if !listingIDRe.MatchString(l.id) || !iok || !bok || !sok || !wok {
		return nil, false
	}
	l.badNames = int(bad)
	if n, ok := extract.Int(c["count"]); ok {
		l.count = &n
	}
	for _, s := range skills {
		item, ok := carriedListedSkill(s)
		if !ok {
			return nil, false
		}
		l.skills = append(l.skills, item)
	}
	for _, k := range listingWhere {
		switch w := where[k].(type) {
		case string, bool:
			l.where[k] = w
		}
	}
	return l, true
}

// carriedListedSkill is one carried {skill, digest} item, checked as listingOf made it.
func carriedListedSkill(v any) (map[string]any, bool) {
	m, _ := v.(map[string]any)
	name, _ := m["skill"].(string)
	if _, ok := skill.ParseName(name); !ok {
		return nil, false
	}
	item := map[string]any{"skill": name}
	if d, has := m["digest"]; has {
		ds, _ := d.(string)
		if !digestRe.MatchString(ds) {
			return nil, false
		}
		item["digest"] = ds
	}
	return item, true
}

// keepEarlier keeps the calls read before the usage offset, for placing listings.
func (s *transcriptScan) keepEarlier() {
	for _, id := range s.earlierOrder {
		k := s.earlierBest[id]
		if msg, _ := k.rec["message"].(map[string]any); msg["model"] != syntheticModel {
			s.res.earlier = append(s.res.earlier, *k)
		}
	}
}
