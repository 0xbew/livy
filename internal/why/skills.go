package why

// How a skill was used, as `livy pr` and `livy profile` say it.
const (
	SkillMethodTool  = "skill_tool" // the agent called the Skill tool
	SkillMethodRead  = "read"       // the agent read the skill's SKILL.md with the Read tool
	SkillMethodSlash = "slash"      // a person typed /name
)

const (
	eventSkillLoaded = "skill.loaded"
	sourceTranscript = "transcript"
)

// SkillMethod is how a skill.loaded line's skill was used, from its trigger; "" for a trigger
// this build does not know.
func SkillMethod(trigger string) string {
	return map[string]string{"agent": SkillMethodTool, "read": SkillMethodRead, "slash": SkillMethodSlash}[trigger]
}

// SkillLoads is the skill.loaded lines among events, each use once, in their order. The hook and
// the transcript both write a Skill tool call's line; the two join on session and tool use id,
// and the hook's is kept, as it carries the SKILL.md's hash.
func SkillLoads(events []Event) []Event {
	key := func(e Event) string { return e.SessionID + "\x00" + e.ToolUseID }
	hooked := map[string]bool{}
	for _, e := range events {
		if e.Event == eventSkillLoaded && e.Source != sourceTranscript && e.ToolUseID != "" {
			hooked[key(e)] = true
		}
	}
	var out []Event
	seen := map[string]bool{}
	for _, e := range events {
		if e.Event != eventSkillLoaded {
			continue
		}
		if e.Source == sourceTranscript && e.ToolUseID != "" {
			if hooked[key(e)] || seen[key(e)] {
				continue
			}
			seen[key(e)] = true
		}
		out = append(out, e)
	}
	return out
}
