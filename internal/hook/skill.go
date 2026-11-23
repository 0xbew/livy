package hook

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/0xbew/livy/internal/gitinfo"
	"github.com/0xbew/livy/internal/pytext"
	"github.com/0xbew/livy/internal/skill"
)

// skill.loaded is written from two hooks: PostToolUse on the Skill tool (the model loaded
// it) and UserPromptSubmit (a person typed /name). Neither payload carries the text Claude
// Code injected, so the hash is of SKILL.md on disk, read right after the load. Never
// written: the skill's text, a typed prompt's arguments, a path, a plugin or folder name.
const (
	skillLoadedEvent  = "skill.loaded"
	triggerAgent      = "agent"
	triggerSlash      = "slash"
	hashDiskAfterLoad = "disk-after-load"
	harnessClaude     = "claude-code"
)

// skillLoad is what a hook payload says was loaded; name "" means nothing was.
type skillLoad struct {
	trigger, name, toolUseID string
	// agentID and agentType are set only when a subagent loaded the skill: Claude Code puts
	// agent_id and agent_type on a hook payload from inside a subagent, and on no other.
	agentID, agentType string
}

// recordSkill writes skill.loaded for the skill this hook saw load, and a ledger.error for
// a payload whose shape changed or a line it could not hash. A prompt that is not a typed
// skill with a SKILL.md on disk (a typed plugin skill among them), and a load that failed,
// leave nothing.
func (r *run) recordSkill(overrides map[string]string, p payload) {
	if p.problem != "" {
		r.errorEvent(p.problem+" (skill.loaded not recorded)", true)
		return
	}
	load, problem := readSkillLoad(p.obj)
	if problem != "" {
		r.errorEvent("skill.loaded not recorded: "+problem, true)
		return
	}
	if load.name == "" {
		return
	}
	n, ok := skill.ParseName(load.name)
	if !ok && load.trigger == triggerAgent {
		r.errorEvent("skill.loaded not recorded: the Skill tool's skill is not a plain name", true)
	}
	if !ok {
		return
	}
	found, resolved := skill.Resolve(n, r.skillPlaces())
	if load.trigger == triggerSlash && found.Dir == "" {
		// Only a SKILL.md on disk proves typed text named a skill; `/re:renewal-call` is a
		// sentence, and its words would otherwise ride along as a plugin skill's name. So a
		// typed plugin skill is not recorded, and a timeout is said without the typed name.
		if !resolved && r.git.TimedOut() {
			r.errorEvent("skill.loaded: git timed out looking for a typed /name's folder-scoped skill; not recorded", true)
		}
		return
	}
	rec := r.build(skillLoadedEvent, overrides, p)
	rec["trigger"] = load.trigger
	rec["harness"] = harnessClaude
	if load.toolUseID != "" {
		rec["tool_use_id"] = load.toolUseID
	}
	// Checked as the launcher tags are: a value that is not a plain token is said, without it.
	switch {
	case load.agentID == "":
	case agentID.MatchString(load.agentID):
		rec["agent_id"] = load.agentID
	default:
		r.errorEvent("skill.loaded: the payload's agent_id is not an agent id; recorded without it", true)
	}
	switch {
	case load.agentType == "":
	case roleToken.MatchString(load.agentType):
		rec["subagent_type"] = load.agentType
	default:
		r.errorEvent("skill.loaded: the payload's agent_type is not a plain token; recorded without it", true)
	}
	why := fillSkill(rec, n, found, resolved)
	if !resolved && r.git.TimedOut() {
		why = "git timed out looking for a folder-scoped skill; recorded without a hash"
	}
	if !r.append(rec) {
		r.errorEvent("could not write event "+skillLoadedEvent, true)
		return
	}
	if why != "" {
		r.errorEvent("skill.loaded "+n.Base+": "+why, true)
	}
}

// readSkillLoad reads the load from a PostToolUse or UserPromptSubmit payload. A missing
// field Claude Code has always sent is a problem, never a guess: its format changed.
func readSkillLoad(obj map[string]any) (skillLoad, string) {
	switch hookName, _ := obj["hook_event_name"].(string); hookName {
	case "PostToolUse":
		return readSkillTool(obj)
	case "UserPromptSubmit":
		prompt, ok := obj["prompt"].(string)
		if !ok {
			return skillLoad{}, "the UserPromptSubmit payload has no prompt string (Claude Code's payload changed?)"
		}
		return skillLoad{trigger: triggerSlash, name: typedSkill(prompt)}, ""
	default:
		return skillLoad{}, "hook_event_name " + pytext.Repr(hookName) + " is not PostToolUse or UserPromptSubmit"
	}
}

// readSkillTool is the Skill tool's load: its skill, and nothing when the load failed.
func readSkillTool(obj map[string]any) (skillLoad, string) {
	if tool, _ := obj["tool_name"].(string); tool != "Skill" {
		return skillLoad{}, "PostToolUse for tool " + pytext.Repr(tool) + ", not Skill"
	}
	input, _ := obj["tool_input"].(map[string]any)
	name, _ := input["skill"].(string)
	if pytext.Strip(name) == "" {
		return skillLoad{}, "the Skill payload has no tool_input.skill (Claude Code's payload changed?)"
	}
	response, _ := obj["tool_response"].(map[string]any)
	success, ok := response["success"].(bool)
	if !ok {
		return skillLoad{}, "the Skill payload has no tool_response.success (Claude Code's payload changed?)"
	}
	if !success {
		return skillLoad{}, ""
	}
	id, _ := obj["tool_use_id"].(string)
	agentID, _ := obj["agent_id"].(string)
	agentType, _ := obj["agent_type"].(string)
	return skillLoad{trigger: triggerAgent, name: pytext.Strip(name), toolUseID: id, agentID: agentID, agentType: agentType}, ""
}

// typedSkill is the name a prompt typed as `/name args`, or "". Only the name is taken:
// the arguments are prompt text.
func typedSkill(prompt string) string {
	rest, ok := strings.CutPrefix(prompt, "/")
	if !ok {
		return ""
	}
	if i := strings.IndexFunc(rest, pytext.IsSpace); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// fillSkill adds the skill's name, scope and hashes to rec, and returns why a hash is
// missing ("" when none is). The reason never names a path.
func fillSkill(rec map[string]any, n skill.Name, found skill.Found, resolved bool) string {
	if !resolved {
		rec["skill"] = n.Base
		return "no SKILL.md where Claude Code looks (a built-in skill, or a place Livy does not read); recorded without a hash"
	}
	rec["skill"] = skill.LineName(n, found)
	rec["scope"] = string(found.Scope)
	if found.Folder != "" {
		rec["scope_dir_hash"] = skill.HashString(found.Folder)
	}
	switch {
	case found.Ambiguous:
		return "more than one folder holds a skill of this name; recorded without a hash"
	case found.Dir == "":
		return "a plugin skill's files are not read; recorded without a hash"
	}
	h, err := skill.HashDir(found.Dir)
	if err != nil {
		return "SKILL.md could not be read; recorded without a hash"
	}
	rec["sha256"] = h.SkillMD
	rec["hash_method"] = hashDiskAfterLoad
	switch {
	case errors.Is(h.TreeErr, skill.ErrTreeTooLarge):
		return "the skill folder is too large to hash in a hook; tree_sha256 left out"
	case h.TreeErr != nil:
		return "the skill folder could not be read in full; tree_sha256 left out"
	}
	rec["tree_sha256"] = h.Tree
	return ""
}

// skillPlaces are where Claude Code looks for skills for this hook's working directory.
func (r *run) skillPlaces() skill.Places {
	cwd, _ := gitinfo.Cwd()
	top := r.git.Toplevel()
	return skill.Places{Managed: claudeManagedDir(runtime.GOOS), User: envOrHome("CLAUDE_CONFIG_DIR", ".claude"), Cwd: cwd, Toplevel: top,
		Nested: func(base string) []string { return r.nestedSkillFolders(top, base) }}
}

// claudeManagedDir is Claude Code's managed settings folder on goos.
func claudeManagedDir(goos string) string {
	switch goos {
	case "darwin":
		return "/Library/Application Support/ClaudeCode"
	case "windows":
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			return filepath.Join(pf, "ClaudeCode")
		}
		return ""
	default:
		return "/etc/claude-code"
	}
}

// nestedSkillFolders is the repo-relative folders below top (tracked or not, ignored files
// left out) that hold .claude/skills/<base>/SKILL.md.
func (r *run) nestedSkillFolders(top, base string) []string {
	out := r.git.Run("-C", top, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--",
		":(glob)**/.claude/skills/*/SKILL.md")
	suffix := "/.claude/skills/" + base + "/SKILL.md"
	seen := map[string]bool{}
	var folders []string
	for p := range strings.SplitSeq(out, "\x00") {
		folder, ok := strings.CutSuffix(p, suffix)
		if ok && folder != "" && !seen[folder] {
			seen[folder] = true
			folders = append(folders, folder)
		}
	}
	return folders
}
