// Package spotcheck is Livy's log-only spot-check: for a shell command or a subagent task
// it asks Jev (TypeSafe's typed-decision model) a frozen set of questions and writes what it
// WOULD decide as a check.decision receipt. It never answers the harness, so it cannot
// change what the agent does.
//
// What it guarantees:
//   - Off without a key: no key, no call, no record.
//   - Scrubbed before send (github.com/0xbew/scrub), and the exact body is audited before
//     the connection opens; a refusal sends nothing and is recorded with the rule that fired.
//   - The ledger keeps verdicts, scores and a hash of what was sent: never the command or brief.
//   - No answer is never a verdict: a failed check is a receipt with verdict "error", and the
//     checks then pause on this machine.
package spotcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/0xbew/scrub"

	"github.com/0xbew/livy/internal/pytext"
)

const (
	briefChars   = 4000
	commandChars = 6000
)

// Planned is what to send for one tool call, scrubbed, or why nothing is.
type Planned struct {
	Check, Contract string
	State           obj
	// OpaqueScript is set for a shell command only.
	OpaqueScript *bool
	SubagentType *string
	// Skipped is why nothing is sent for a subagent brief.
	Skipped string
}

// pyStr is str(v or "") for a decoded JSON value.
func pyStr(v any) string {
	if !pytext.Truthy(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Plan is what to send for this tool call, or nil when there is nothing to check. repo is
// the slug the shell check names.
func Plan(payload map[string]any, d *scrub.Denylist, repo string) *Planned {
	tool, _ := payload["tool_name"].(string)
	input, _ := payload["tool_input"].(map[string]any)
	switch tool {
	case "Bash":
		command, ok := input["command"].(string)
		if !ok || pytext.Strip(command) == "" {
			return nil
		}
		opaque := OpaqueScript(command)
		return &Planned{Check: "guard", Contract: "guard@1", OpaqueScript: &opaque, State: obj{
			{"repo", repo},
			{"command", scrub.Scrub(command, d, commandChars)},
			{"agent_description", scrub.Scrub(pyStr(input["description"]), d, commandChars)}}}
	case "Agent", "Task":
		brief, description := pyStr(input["prompt"]), pyStr(input["description"])
		if pytext.Strip(brief) == "" {
			return nil
		}
		sub := pyStr(input["subagent_type"])
		if sub == "" {
			sub = "general-purpose"
		}
		p := &Planned{Check: "route", Contract: "route@1", SubagentType: &sub}
		transcript, _ := payload["transcript_path"].(string)
		prompts, ok := OwnerPrompts(transcript)
		switch {
		case !ok:
			p.Skipped = "transcript unreadable; cannot rule out the engineer's own words"
		case len(prompts) == 0:
			// A subagent's own transcript, or a session with no human prompt yet: nothing to
			// compare against is not the same as "nothing repeated".
			p.Skipped = "no engineer prompts in this transcript to compare against"
		case RepeatsOwner(description+"\n"+brief, prompts):
			p.Skipped = "brief repeats the engineer's own prompt"
		default:
			p.State = obj{
				{"subagent_type", sub},
				{"description", scrub.Scrub(description, d, commandChars)},
				{"brief", scrub.Scrub(brief, d, briefChars)}}
		}
		return p
	}
	return nil
}

// InputSHA is the 16 hex characters the receipt keeps of what was sent: the hash of the
// state as json.dumps writes it with its keys sorted.
func InputSHA(state obj) string {
	sorted := append(obj(nil), state...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].K < sorted[j].K })
	sum := sha256.Sum256([]byte(dumps(sorted)))
	return hex.EncodeToString(sum[:])[:16]
}

// A command whose effect lives in a file Jev cannot see. Recorded as a flag beside the
// frozen policy, not folded into it, so the live verdict stays comparable with the test.
// The pattern needs a lookahead, which RE2 has not: OpaqueScript reads that part by hand.
const ws = `[\s\x0b\x1c-\x1f\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

var (
	opaqueShell  = regexp.MustCompile(`^(?:bash|sh|zsh)` + ws + `+(\S+)`)
	opaqueDot    = regexp.MustCompile(`^\./[\p{L}\p{N}_./-]+`)
	opaqueMake   = regexp.MustCompile(`^(?:make|just)`)
	opaqueRunner = regexp.MustCompile(`^(?:npm|pnpm|yarn)` + ws + `+run`)
)

func startsWord(rest string) bool {
	for _, r := range rest {
		return pytext.IsWord(r)
	}
	return false
}

// OpaqueScript reports whether command runs a script or task runner by name.
func OpaqueScript(command string) bool {
	var prev rune
	for i, r := range command {
		start := i == 0 || strings.ContainsRune(";&|(", prev) || pytext.IsSpace(prev)
		prev = r
		if !start {
			continue
		}
		rest := command[i:]
		if m := opaqueShell.FindStringSubmatch(rest); m != nil {
			// `bash -c "..."` runs text Jev can see; `-cx` is a different word.
			if !strings.HasPrefix(m[1], "-c") || startsWord(m[1][2:]) {
				return true
			}
		}
		if opaqueDot.MatchString(rest) {
			return true
		}
		if m := opaqueMake.FindString(rest); m != "" && !startsWord(rest[len(m):]) {
			return true
		}
		if m := opaqueRunner.FindString(rest); m != "" && !startsWord(rest[len(m):]) {
			return true
		}
	}
	return false
}
