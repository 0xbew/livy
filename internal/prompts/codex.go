package prompts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"

	"github.com/0xbew/livy/internal/extract"
	"github.com/0xbew/livy/internal/extract/claudecode"
	"github.com/0xbew/livy/internal/pytext"
)

// Codex writes its own scaffolding (instructions, environment, permissions) as user
// messages. Those blocks are the harness's words, not the person's.
var codexHarnessBlocks = []string{"<environment_context", "<user_instructions", "# AGENTS.md instructions",
	"<INSTRUCTIONS", "<permissions", "<collaboration_mode", "<skills", "<turn_aborted", "<subagent", "<user_action",
	"<system-reminder", "<ide_"}

func codexText(content any, blockType string) string {
	list, _ := content.([]any)
	var parts []string
	for _, b := range list {
		m, _ := b.(map[string]any)
		t, _ := m["text"].(string)
		if m["type"] != blockType || t == "" {
			continue
		}
		lead := strings.TrimLeftFunc(t, pytext.IsSpace)
		harness := false
		for _, p := range codexHarnessBlocks {
			harness = harness || strings.HasPrefix(lead, p)
		}
		if blockType == "input_text" && harness {
			continue
		}
		parts = append(parts, t)
	}
	return pytext.Strip(strings.Join(parts, " "))
}

func codexSubagent(payload map[string]any) bool {
	if payload["thread_source"] == "subagent" {
		return true
	}
	src, ok := payload["source"].(map[string]any)
	_, sub := src["subagent"]
	return ok && sub
}

// codexPrompts is the human prompts of one Codex rollout at or after byte start, as
// claudecode.PromptsIn reads a Claude Code transcript: same context rule, same exchanges.
func codexPrompts(scope *extract.Scope, path string, start int64) (claudecode.PromptRead, error) {
	cr := &codexPromptReader{scope: scope, start: start}
	offset, bad, err := extract.ReadRecords(path, cr.record)
	if err != nil {
		return claudecode.PromptRead{}, err
	}
	return claudecode.PromptRead{Found: cr.found, Offset: offset, Bad: bad, NoSession: cr.noSession, Outside: cr.outside}, nil
}

// codexPromptReader is one read of a rollout: where the human is, and the last exchanges.
type codexPromptReader struct {
	scope *extract.Scope
	start int64

	session, repo, branch, lastAgent string
	exchanges                        claudecode.Exchanges
	subagent                         bool

	found     []claudecode.Prompt
	outside   []claudecode.Prompt
	noSession []string
}

func (cr *codexPromptReader) record(rec map[string]any, here int64) {
	payload, _ := rec["payload"].(map[string]any)
	switch rec["type"] {
	case "session_meta":
		cr.sessionMeta(payload)
	case "turn_context":
		if cwd, ok := payload["cwd"].(string); ok {
			cr.repo = cr.scope.RepoFor(cwd)
		}
	case "response_item":
		cr.message(rec, payload, here)
	}
}

func (cr *codexPromptReader) sessionMeta(payload map[string]any) {
	if s, ok := payload["id"].(string); ok {
		cr.session = s
	}
	cr.subagent = codexSubagent(payload)
	if g, ok := payload["git"].(map[string]any); ok {
		cr.branch, _ = g["branch"].(string)
	}
	if cwd, ok := payload["cwd"].(string); ok {
		cr.repo = cr.scope.RepoFor(cwd)
	}
}

func (cr *codexPromptReader) message(rec, payload map[string]any, here int64) {
	if payload["type"] != "message" || cr.subagent {
		return
	}
	switch payload["role"] {
	case "assistant":
		if t := codexText(payload["content"], "output_text"); t != "" && cr.repo != "" {
			cr.lastAgent = t
			cr.exchanges.AgentSaid(t)
		}
	case "user":
		cr.human(rec, payload, here)
	}
}

// human takes a person's message: a prompt when past start and in a repo, an outside one
// anywhere outside every repo, and the owner of the exchange that follows.
func (cr *codexPromptReader) human(rec, payload map[string]any, here int64) {
	human := codexText(payload["content"], "input_text")
	if human == "" {
		return
	}
	switch {
	case cr.repo == "" && cr.session != "":
		cr.outside = append(cr.outside, claudecode.Prompt{ID: cr.promptID(rec, human), TS: rec["timestamp"],
			SessionID: cr.session, Text: human})
	case cr.repo != "" && here >= cr.start:
		cr.prompt(rec, human)
	}
	cr.exchanges.PersonSaid(human, cr.repo != "")
	if cr.repo == "" {
		cr.lastAgent = ""
	}
}

// prompt keeps one prompt with its last three exchanges, or notes it has no session.
func (cr *codexPromptReader) prompt(rec map[string]any, human string) {
	if cr.session == "" {
		sum := sha256.Sum256([]byte(extract.PyStr(rec["timestamp"]) + "|" + human))
		cr.noSession = append(cr.noSession, "prompt:"+hex.EncodeToString(sum[:])[:24])
		return
	}
	p := claudecode.Prompt{ID: cr.promptID(rec, human), TS: rec["timestamp"],
		Repo: cr.repo, Branch: cr.branch, SessionID: cr.session, Text: human, AgentBefore: cr.lastAgent}
	p.Exchanges = append(p.Exchanges, cr.exchanges.Last()...)
	cr.found = append(cr.found, p)
}

// promptID is prompt capture's id for a human message of this rollout's session.
func (cr *codexPromptReader) promptID(rec map[string]any, human string) string {
	sum := sha256.Sum256([]byte(cr.session + "|" + extract.PyStr(rec["timestamp"]) + "|" + human))
	return "prompt:codex-" + hex.EncodeToString(sum[:])[:24]
}

// addCodexSources reads one rollout's agent messages as relay sources.
func (s *Sources) addCodexFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		s.Problems["file_unreadable"]++
		return
	}
	var session, cwd string
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		line := data[:i+1]
		data = data[i+1:]
		v, err := extract.Decode([]byte(pytext.DecodeReplace(line)))
		rec, ok := v.(map[string]any)
		if err != nil || !ok {
			s.Problems["bad_json"]++
			continue
		}
		s.addCodexRecord(rec, &session, &cwd)
	}
}

// addCodexRecord reads one rollout record: the session and folder it names, or an agent
// message as a relay source.
func (s *Sources) addCodexRecord(rec map[string]any, session, cwd *string) {
	payload, _ := rec["payload"].(map[string]any)
	switch rec["type"] {
	case "session_meta":
		*session, _ = payload["id"].(string)
		if c, ok := payload["cwd"].(string); ok {
			*cwd = c
		}
	case "turn_context":
		if c, ok := payload["cwd"].(string); ok {
			*cwd = c
		}
	case "response_item":
		if payload["type"] != "message" || payload["role"] != "assistant" {
			return
		}
		if *session == "" {
			s.Problems["no_session"]++
			return
		}
		when, ok, naive := aware(rec["timestamp"])
		if naive {
			s.Problems["source_time_naive"]++
			return
		}
		if !ok {
			s.Problems["source_time_unreadable"]++
			return
		}
		s.addText(*session, when, *cwd, codexText(payload["content"], "output_text"))
	}
}
