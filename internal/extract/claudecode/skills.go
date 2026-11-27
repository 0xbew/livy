package claudecode

import (
	"path/filepath"
	"strings"

	"github.com/0xbew/livy/internal/extract"
	"github.com/0xbew/livy/internal/pytext"
	"github.com/0xbew/livy/internal/skill"
)

// skill.loaded from the transcript: a skill an agent used, by calling the Skill tool or by reading
// a SKILL.md with the Read tool. The hook writes the Skill tool's loads too, but only when Claude
// Code's own folder is in an in-scope repo, and it never sees a Read; a session started outside
// the repo leaves every PR at no skill. Names and ids only: never the skill's text, a path or a
// folder name. A load whose result was an error is not a load.
//
// Where the line goes: the call's own folder (or the folder it named), else the skill's folder
// when that is in an in-scope repo, else the repo of the same agent's next call.
// With none, the load is out of scope and leaves no trace.

// Skill use triggers, as skill.loaded's trigger field says them.
const (
	triggerSkillTool = "agent" // the Skill tool, as the hook writes it
	triggerRead      = "read"  // the Read tool on a SKILL.md
	skillHarness     = "claude-code"
	skillDirPrefix   = "Base directory for this skill: "
	skillFileName    = "SKILL.md"
)

// skillUse is one tool call that used a skill: how, the name as the line carries it, and the
// skill's folder when the transcript says it ("" otherwise; never written).
type skillUse struct {
	trigger, name, dir string
}

// skillUseOf is the skill a tool call used, if any. A Read counts only on a file named SKILL.md
// in a folder under a `skills` folder, as Claude Code lays skills out.
func skillUseOf(tool string, input any, cwd string) (skillUse, bool) {
	in, _ := input.(map[string]any)
	switch tool {
	case "Skill":
		raw, _ := in["skill"].(string)
		n, ok := skill.ParseName(pytext.Strip(raw))
		if !ok {
			return skillUse{}, false
		}
		return skillUse{trigger: triggerSkillTool, name: lineName(n)}, true
	case "Read":
		p, _ := in["file_path"].(string)
		if p == "" {
			return skillUse{}, false
		}
		if !filepath.IsAbs(p) {
			if !filepath.IsAbs(cwd) {
				return skillUse{}, false
			}
			p = filepath.Join(cwd, p)
		}
		dir := filepath.Dir(filepath.Clean(p))
		base := filepath.Base(dir)
		if filepath.Base(p) != skillFileName || filepath.Base(filepath.Dir(dir)) != "skills" {
			return skillUse{}, false
		}
		n, ok := skill.ParseName(base)
		if !ok || n.Prefix != "" {
			return skillUse{}, false
		}
		return skillUse{trigger: triggerRead, name: n.Base, dir: dir}, true
	}
	return skillUse{}, false
}

// lineName is a Skill call's name as a line carries it: a plugin's or a folder's prefix only when
// it is a public plugin's, any other prefix as its sha256, as the hook writes a plugin skill.
func lineName(n skill.Name) string {
	if n.Prefix == "" {
		return n.Base
	}
	return skill.LineName(n, skill.Found{Scope: skill.ScopePlugin})
}

// skillDirOf is the folder a Skill call's skill is in, from the message Claude Code adds after
// the call loads it (isMeta, naming the call): "" when the record is not that message.
func skillDirOf(rec map[string]any, content []any) (id, dir string) {
	if !pytext.Truthy(rec["isMeta"]) {
		return "", ""
	}
	id, _ = rec["sourceToolUseID"].(string)
	if !toolIDRe.MatchString(id) || len(content) == 0 {
		return "", ""
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	rest, ok := strings.CutPrefix(text, skillDirPrefix)
	if !ok {
		return "", ""
	}
	line, _, _ := strings.Cut(rest, "\n")
	if !filepath.IsAbs(line) {
		return "", ""
	}
	return id, filepath.Clean(line)
}

// skillLine is a skill use's skill.loaded line, placed at repo and branch.
func skillLine(it *outcomeItem, repo, branch, ts string) map[string]any {
	line := base("skill.loaded", repo, ts)
	line["branch"] = branch
	line["skill"] = it.skill.name
	line["trigger"] = it.skill.trigger
	line["harness"] = skillHarness
	line["tool_use_id"] = it.toolUseID
	setStr(line, "session_id", it.where["sessionId"])
	setStr(line, "agent_id", it.where["agentId"])
	return line
}

// skillLines adds a skill.loaded line for each skill use among p's outcomes that worked: placed as
// a tool.outcome is (its folder, or one it named), else by the skill's folder, else by the same
// agent's next call in fl. In a file that may still grow, a use with none of these waits in the
// outcome carry for the agent's next call; in a settled one it was out of scope.
func (ps *extractPass) skillLines(p *parsed, fl *fileLines, settled bool) {
	var waiting []any
	for _, it := range p.outcomes {
		if it.skill == nil || it.isError || it.skip != "" || ps.seenSkills[it.toolUseID] {
			continue
		}
		ts := extract.UTCSeconds(it.where["timestamp"])
		if ts == "" {
			continue
		}
		repo, branch := "", extract.Unknown
		if placed, why := ps.placeOutcome(it); why == "" {
			repo, branch = placed.Repo, branchOf(it.where, placed)
		} else if r := ps.skillFolderRepo(it.skill.dir); r != "" {
			repo = r
		} else {
			repo, branch = nextCall(fl.lines, it.where, ts)
		}
		switch {
		case repo == "" && !settled && len(waiting) < outcomeCarryCap:
			waiting = append(waiting, []any{it.toolUseID, it.skill.trigger, it.skill.name, it.skill.dir,
				it.where["sessionId"], it.where["agentId"], it.where["timestamp"]})
		case repo == "":
			ps.stats["skills_out_of_scope"]++
		default:
			fl.markSeen(ps.seenSkills, it.toolUseID)
			fl.lines = append(fl.lines, skillLine(it, repo, branch, ts))
			ps.stats["skills_used"]++
		}
	}
	if p.outcomeCarry != nil {
		p.outcomeCarry["unplaced_skills"] = waiting
	}
}

// waitingSkills are the skill uses the last pass could not place yet, as skill-only outcomes, in
// exactly the shape skillLines carries them.
func waitingSkills(carry any) []*outcomeItem {
	c, _ := carry.(map[string]any)
	list, _ := c["unplaced_skills"].([]any)
	var out []*outcomeItem
	for _, v := range list {
		parts, ok := v.([]any)
		if !ok || len(parts) != 7 || len(out) >= outcomeCarryCap {
			continue
		}
		id, _ := parts[0].(string)
		u, ok := carriedSkill(parts[1:4])
		session, _ := parts[4].(string)
		if !ok || !toolIDRe.MatchString(id) || session == "" {
			continue
		}
		where := map[string]any{"sessionId": session, "timestamp": parts[6]}
		if agent, isStr := parts[5].(string); isStr {
			where["agentId"] = agent
		}
		out = append(out, &outcomeItem{index: -1, toolUseID: id, where: where, quiet: true, skill: &u})
	}
	return out
}

// skillFolderRepo is the in-scope repo holding a skill's folder, "" when none does.
func (ps *extractPass) skillFolderRepo(dir string) string {
	if dir == "" {
		return ""
	}
	if r := ps.scope.RepoFor(dir); r != extract.Unknown {
		return r
	}
	return ""
}

// nextCall is the repo and branch of the first usage line in lines at or after ts made by the same
// session and agent as where: the work the skill was used for. A call before the use is never
// taken, so a pass cut anywhere places a use the same way.
func nextCall(lines []map[string]any, where map[string]any, ts string) (repo, branch string) {
	session, _ := where["sessionId"].(string)
	agent, _ := where["agentId"].(string)
	var found map[string]any
	for _, l := range lines {
		if l["event"] != "usage.api_call" || l["session_id"] != session || stringField(l, "agent_id") != agent {
			continue
		}
		if at, _ := l["ts"].(string); at >= ts && (found == nil || at < found["ts"].(string)) {
			found = l
		}
	}
	if found == nil || session == "" {
		return "", ""
	}
	return stringField(found, "repo"), stringField(found, "branch")
}

func stringField(l map[string]any, k string) string {
	s, _ := l[k].(string)
	return s
}

// skillOf is the skill call id used, with the folder Claude Code named for it; nil for none.
func (s *transcriptScan) skillOf(id string) *skillUse {
	u, ok := s.skillUses[id]
	if !ok {
		return nil
	}
	if d := s.skillDirs[id]; d != "" {
		u.dir = d
	}
	return &u
}

// carriedSkills is the skill uses among the calls the last pass left waiting for a result, in
// exactly the shape outcomeCarryAt writes them: [trigger, name, folder].
func carriedSkills(carry any, names map[string]string) map[string]skillUse {
	out := map[string]skillUse{}
	c, _ := carry.(map[string]any)
	skills, _ := c["skills"].(map[string]any)
	for id, v := range skills {
		parts, ok := v.([]any)
		if _, pending := names[id]; !pending || !ok {
			continue
		}
		if u, ok := carriedSkill(parts); ok {
			out[id] = u
		}
	}
	return out
}

// carriedSkill reads [trigger, name, folder] from a state file.
func carriedSkill(parts []any) (skillUse, bool) {
	if len(parts) != 3 {
		return skillUse{}, false
	}
	trigger, _ := parts[0].(string)
	name, _ := parts[1].(string)
	dir, _ := parts[2].(string)
	if (trigger != triggerSkillTool && trigger != triggerRead) || !extract.IsName(name) || (dir != "" && !filepath.IsAbs(dir)) {
		return skillUse{}, false
	}
	return skillUse{trigger: trigger, name: name, dir: dir}, true
}
