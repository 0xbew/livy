package pr

import (
	"cmp"
	"slices"
	"time"

	"github.com/0xbew/livy/internal/why"
)

const eventSkillsListed = "skills.listed"

// SkillsInContext is which skills the pull request's sessions had in context: Claude Code listed
// them to the model as available, whether or not one was used.
type SkillsInContext struct {
	// Sessions is how many sessions the commits' windows hold; Listed how many of them have a skill
	// listing in the ledger by their windows' end.
	Sessions int `json:"sessions"`
	Listed   int `json:"listed"`
	// Skills are the skills listed or used, used first, then by how many sessions listed them.
	Skills []SkillInContext `json:"skills"`
}

// SkillInContext is one skill: how many of the sessions had it listed, whether one of them used it
// (a skill.loaded line in a window), and for a used skill the digest its listing gave.
type SkillInContext struct {
	Skill    string `json:"skill"`
	Sessions int    `json:"sessions"`
	Used     bool   `json:"used"`
	Digest   string `json:"digest"`
}

// skillsInContext is the skills the windows' sessions had listed by each session's last window end,
// with the skills used in them (skills).
func skillsInContext(explained []why.Explained, events []why.Event, used []Skill) SkillsInContext {
	ends := map[string]time.Time{}
	for _, ex := range explained {
		for _, s := range ex.Report.Sessions {
			if end, seen := ends[s.ID]; !seen || s.To.After(end) {
				ends[s.ID] = s.To
			}
		}
	}
	// per session: each listed skill, with the digest its latest listing gave, kept over a listing that gave none
	listed := map[string]map[string]string{}
	for _, e := range events {
		end, ok := ends[e.SessionID]
		t, tok := why.ParseTS(e.TS)
		if e.Event != eventSkillsListed || !ok || !tok || t.After(end) {
			continue
		}
		if listed[e.SessionID] == nil {
			listed[e.SessionID] = map[string]string{}
		}
		for _, s := range e.ListedSkills {
			if s.Digest != "" || listed[e.SessionID][s.Skill] == "" {
				listed[e.SessionID][s.Skill] = s.Digest
			}
		}
	}
	return contextOf(len(ends), listed, used)
}

// contextOf counts each skill across the sessions' listings and marks the ones used.
func contextOf(sessions int, listed map[string]map[string]string, used []Skill) SkillsInContext {
	byName := map[string]*SkillInContext{}
	entry := func(name string) *SkillInContext {
		if byName[name] == nil {
			byName[name] = &SkillInContext{Skill: name}
		}
		return byName[name]
	}
	for _, skills := range listed {
		for name := range skills {
			entry(name).Sessions++
		}
	}
	for _, u := range used {
		e := entry(u.Skill)
		e.Used = true
		if d := listed[u.SessionID][u.Skill]; d != "" && e.Digest == "" {
			e.Digest = d
		}
	}
	out := SkillsInContext{Sessions: sessions, Listed: len(listed), Skills: []SkillInContext{}}
	for _, e := range byName {
		out.Skills = append(out.Skills, *e)
	}
	slices.SortFunc(out.Skills, func(a, b SkillInContext) int {
		if a.Used != b.Used {
			if a.Used {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(b.Sessions, a.Sessions), cmp.Compare(a.Skill, b.Skill))
	})
	return out
}
