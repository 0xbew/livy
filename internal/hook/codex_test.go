//go:build unix

package hook

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexCase is one hook call from a real Codex session: the Livy event install wires to
// the Codex hook, and the payload Codex sent (text fields replaced, paths templated).
type codexCase struct {
	Event    string         `json:"event"`
	ExitCode int            `json:"exit_code"`
	Payload  map[string]any `json:"payload"`
}

// replayCodex runs each case's payload through `livy hook --harness codex <event>` in an
// in-scope repo and returns the ledger lines and the raw ledger.
func replayCodex(t *testing.T, fixture string) ([]map[string]any, string) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	cases, err := os.ReadFile(filepath.Join(dir, fixture))
	if err != nil {
		t.Fatal(err)
	}
	roll, err := os.ReadFile(filepath.Join(dir, "rollout.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	data := orgRepo(t)
	cwd, _ := os.Getwd()
	transcript := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(transcript, []byte(strings.ReplaceAll(string(roll), "{{cwd}}", cwd)), 0o600); err != nil {
		t.Fatal(err)
	}
	for row := range strings.SplitSeq(strings.TrimSpace(string(cases)), "\n") {
		var c codexCase
		if err := json.Unmarshal([]byte(row), &c); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(c.Payload)
		payload := strings.NewReplacer("{{cwd}}", cwd, "{{transcript}}", transcript).Replace(string(raw))
		if c.Event == "" {
			c.Event = "tool.call"
		}
		if code := Run([]string{"--harness", "codex", c.Event}, stdinWith(t, payload), io.Discard); code != 0 {
			t.Fatalf("exit %d", code)
		}
	}
	ledgerPath := filepath.Join(data, "ledger.jsonl")
	raw, _ := os.ReadFile(ledgerPath)
	return lines(t, ledgerPath), string(raw)
}

// Guards: Codex sessions leave no session, prompt, tool, subagent or turn lines (the hooks
// were never wired), and a subagent's brief recorded as a person's prompt. Input: the hook
// payloads of one real Codex 0.171 session (a shell call, a spawned subagent with its own
// shell call, the wait); expected: what Livy records for Claude Code's same events.
func TestCodexHooksRecordARealSession(t *testing.T) {
	got, raw := replayCodex(t, "session.jsonl")
	const session, turn, subTurn = "01a208b3-f6c4-75c0-90ee-509234bfb947", "01a208b3-f8b6-7042-8b08-f5c489065839", "01a208b4-1b77-7d50-a7ae-035c9be15a5c"
	const sub = "01a208b4-1b57-75e0-ba6e-c5c85619ed08"
	want := []string{"session.started", "prompt.submitted", "tool.call Bash", "tool.call spawn_agent", "agent.launched",
		"tool.call Bash " + sub, "tool.call multi_agent_v1wait_agent", "turn.ended", "session.ended"}
	var summary []string
	for _, l := range got {
		s, _ := l["event"].(string)
		if tool, ok := l["tool"].(string); ok && s == "tool.call" {
			s += " " + tool
		}
		if a, ok := l["agent_id"].(string); ok && s != "agent.launched" {
			s += " " + a
		}
		summary = append(summary, s)
	}
	if strings.Join(summary, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines:\n%s\nwant:\n%s\nledger:\n%s", strings.Join(summary, "\n"), strings.Join(want, "\n"), raw)
	}
	for i, l := range got {
		if l["harness"] != "codex" || l["session_id"] != session || l["repo"] != "acme/widgets" || l["branch"] != "feature/x" {
			t.Errorf("line %d = %v, want harness codex, session %s, acme/widgets on feature/x", i, l, session)
		}
		if i != len(got)-1 && l["model"] != "gpt-5.6-luna" {
			t.Errorf("line %d model = %v, want gpt-5.6-luna (SessionEnd alone carries none)", i, l["model"])
		}
	}
	for _, c := range []struct {
		line  int
		field string
		want  any
	}{
		{0, "reason", "startup"},
		{1, "prompt_id", turn}, {1, "turn_id", turn},
		{2, "tool_use_id", "exec-4d35bccd-7f11-4460-ab3a-1a286894d4d6"}, {2, "prompt_id", turn},
		{4, "agent_id", sub}, {4, "subagent_type", "default"}, {4, "agent_label_status", "absent"},
		{5, "subagent_type", "default"}, {5, "turn_id", subTurn}, {5, "prompt_id", nil},
		{7, "prompt_id", turn}, {7, "stop_hook_active", false},
		{8, "reason", "other"},
	} {
		if got[c.line][c.field] != c.want {
			t.Errorf("line %d (%v) %s = %v, want %v", c.line, got[c.line]["event"], c.field, got[c.line][c.field], c.want)
		}
	}
	if no, _ := got[0]["not_observable"].([]any); len(no) == 0 {
		t.Errorf("session.started not_observable = %v, want the events Codex cannot report", got[0]["not_observable"])
	}
	if strings.Contains(raw, "NEVER RECORDED") || strings.Contains(raw, "rollout") {
		t.Errorf("text or a path reached the ledger:\n%s", raw)
	}
}

// Guards: a `gh pr create` that failed recorded as a PR opened. Codex runs PostToolUse after
// a shell call whatever its exit code, and its payload has none: the call's exit code is read
// from the session's rollout (real item_completed records, exit 0 and exit 1).
func TestCodexPrOpenedOnlyWhenGhPrCreateSucceeded(t *testing.T) {
	got, raw := replayCodex(t, "prcreate.jsonl")
	var opened []string
	for _, l := range got {
		if l["event"] == "work.pr_opened" {
			opened = append(opened, l["tool_use_id"].(string))
			if l["target_repo"] != "acme/widgets" || l["target_branch"] != "feature/x" || l["harness"] != "codex" {
				t.Errorf("work.pr_opened = %v, want target acme/widgets feature/x, harness codex", l)
			}
		}
	}
	if len(opened) != 1 || opened[0] != "exec-2c6f5937-cb03-42de-864e-19dea4dc66a7" {
		t.Errorf("work.pr_opened for %v, want only the call that exited 0\nledger:\n%s", opened, raw)
	}
}
