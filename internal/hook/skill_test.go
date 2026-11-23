//go:build unix

package hook

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A skill load becomes one skill.loaded line naming the skill and the sha256 of its
// SKILL.md bytes (what `shasum -a 256 SKILL.md` prints), and never the arguments a typed
// /name carries.
// Payload shapes are the ones Claude Code 2.1.322 sent in the PR's probe sessions.
func TestSkillLoadRecordsTheFileHashAndNoArguments(t *testing.T) {
	// Guards: skill.loaded hashing something other than the SKILL.md file's bytes (the
	// injected text drops the frontmatter), and a typed prompt's words reaching the ledger.
	const md = "---\nname: demo\ndescription: A demo skill.\n---\nSay hello.\n"
	const wantSHA = "64dda59d7ceab93fa81aa2006e4e52812ce653deb93cee4c8a1c93d77a6320a8"
	for _, tc := range []struct{ name, payload, trigger string }{
		{"agent", `{"session_id":"s1","hook_event_name":"PostToolUse","tool_name":"Skill",` +
			`"tool_input":{"skill":"demo"},"tool_response":{"success":true,"commandName":"demo"},` +
			`"tool_use_id":"toolu_1"}`, "agent"},
		{"slash", `{"session_id":"s1","hook_event_name":"UserPromptSubmit",` +
			`"prompt":"/demo SECRET-ARG please"}`, "slash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := orgRepo(t)
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			dir := filepath.Join(".claude", "skills", "demo")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
				t.Fatal(err)
			}
			Run([]string{"skill.loaded"}, stdinWith(t, tc.payload), io.Discard)
			path := filepath.Join(data, "ledger.jsonl")
			got := lines(t, path)
			if len(got) != 1 || got[0]["event"] != "skill.loaded" || got[0]["skill"] != "demo" ||
				got[0]["sha256"] != wantSHA || got[0]["trigger"] != tc.trigger || got[0]["scope"] != "project" {
				t.Fatalf("got %v, want one skill.loaded for demo, trigger %s, sha256 %s", got, tc.trigger, wantSHA)
			}
			raw, _ := os.ReadFile(path)
			if strings.Contains(string(raw), "SECRET-ARG") || strings.Contains(string(raw), ".claude") {
				t.Errorf("the line carries the prompt's words or a path: %s", raw)
			}
		})
	}
}

// Typed text shaped like `/word:words` names no skill unless a SKILL.md for it is on disk.
func TestTypedTextThatIsNoSkillLeavesNoLine(t *testing.T) {
	// Guards: the words of a prompt like `/re:renewal-call` written as a plugin skill's name.
	data := orgRepo(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	Run([]string{"skill.loaded"}, stdinWith(t, `{"session_id":"s1","hook_event_name":"UserPromptSubmit",`+
		`"prompt":"/re:renewal-call tomorrow"}`), io.Discard)
	if got := lines(t, filepath.Join(data, "ledger.jsonl")); len(got) != 0 {
		t.Fatalf("got %v, want no line", got)
	}
}
