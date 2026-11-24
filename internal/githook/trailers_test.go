package githook

import "testing"

// Guards: a commit credited to the wrong author. A human commit stamped as an agent's, a
// merge or squash message stamped, an amend or an agent's own trailers duplicated (also when
// they sit a paragraph above a Co-Authored-By line), a model invented when none was recorded,
// and a Codex commit credited to the Claude Code session that launched it (Codex inherits
// CLAUDECODE and CLAUDE_CODE_SESSION_ID, read from a real Codex run's environment). Expected
// messages are git interpret-trailers' own output shape.
func TestStampMessage(t *testing.T) {
	claude := map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "s-1"}
	codex := map[string]string{"CODEX_THREAD_ID": "t-1"}
	both := map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "s-1", "CODEX_THREAD_ID": "t-1"}
	known := sessionFacts{model: "claude-opus-5-5", version: "2.1.319"}
	stamped := "fix: thing\n\nAgent-Model: claude-opus-5-5\nAgent-Harness: claude-code@2.1.319\nAgent-Session: s-1\n"
	for _, c := range []struct {
		name, msg, source, mode, want string
		env                           map[string]string
		facts                         sessionFacts
	}{
		{name: "claude code commit", env: claude, source: "message", facts: known, msg: "fix: thing\n", want: stamped},
		{name: "no agent in the environment", env: nil, source: "message", facts: known, msg: "fix: thing\n", want: "fix: thing\n"},
		{name: "CLAUDECODE without a session", env: map[string]string{"CLAUDECODE": "1"}, source: "message", facts: known,
			msg: "fix: thing\n", want: "fix: thing\n"},
		{name: "a session id without CLAUDECODE", env: map[string]string{"CLAUDE_CODE_SESSION_ID": "s-1"}, source: "message",
			facts: known, msg: "fix: thing\n", want: "fix: thing\n"},
		{name: "merge", env: claude, source: "merge", facts: known, msg: "Merge branch 'x'\n", want: "Merge branch 'x'\n"},
		{name: "squash", env: claude, source: "squash", facts: known, msg: "Squashed commit of the following:\n",
			want: "Squashed commit of the following:\n"},
		{name: "amend keeps the trailers once", env: claude, source: "commit", facts: known, msg: stamped, want: stamped},
		{name: "appends to an existing trailer block", env: claude, source: "message", facts: known,
			msg:  "fix: thing\n\nCo-authored-by: A <a@example.test>\n",
			want: "fix: thing\n\nCo-authored-by: A <a@example.test>\nAgent-Model: claude-opus-5-5\nAgent-Harness: claude-code@2.1.319\nAgent-Session: s-1\n"},
		{name: "the agent's own trailer wins", env: claude, source: "message", facts: known,
			msg:  "fix: thing\n\nAgent-Session: mine\n",
			want: "fix: thing\n\nAgent-Session: mine\nAgent-Model: claude-opus-5-5\nAgent-Harness: claude-code@2.1.319\n"},
		{name: "an Agent-* block above a separate Co-Authored-By paragraph: one block, no second Agent-Session",
			env: claude, source: "message", facts: known,
			msg:  "fix: thing\n\nAgent-Session: mine\n\nCo-Authored-By: A\n",
			want: "fix: thing\n\nAgent-Session: mine\nCo-Authored-By: A\nAgent-Model: claude-opus-5-5\nAgent-Harness: claude-code@2.1.319\n"},
		{name: "an agent's Agent-Mode does not hide Agent-Model (git matches keys by prefix)", env: claude, source: "message",
			facts: known, msg: "fix: thing\n\nAgent-Mode: drive\n",
			want: "fix: thing\n\nAgent-Mode: drive\nAgent-Model: claude-opus-5-5\nAgent-Harness: claude-code@2.1.319\nAgent-Session: s-1\n"},
		{name: "unknown session: no model, no harness", env: claude, source: "message", facts: sessionFacts{},
			msg: "fix: thing\n", want: "fix: thing\n\nAgent-Session: s-1\n"},
		{name: "configured mode", env: claude, source: "message", mode: "drive", facts: known, msg: "fix: thing\n",
			want: "fix: thing\n\nAgent-Model: claude-opus-5-5\nAgent-Harness: claude-code@2.1.319\nAgent-Session: s-1\nAgent-Mode: drive\n"},
		{name: "codex commit", env: codex, source: "message", facts: sessionFacts{version: "0.167.0"}, msg: "fix: thing\n",
			want: "fix: thing\n\nAgent-Harness: codex@0.167.0\nAgent-Session: t-1\n"},
		{name: "codex launched from claude code: cannot tell which committed", env: both, source: "message", facts: known,
			msg: "fix: thing\n", want: "fix: thing\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			getenv := func(k string) string { return c.env[k] }
			lookup := func(agent) sessionFacts { return c.facts }
			got, err := stampMessage([]byte(c.msg), getenv, c.source, lookup, c.mode)
			if err != nil {
				t.Fatalf("stampMessage(%q): %v", c.msg, err)
			}
			if string(got) != c.want {
				t.Errorf("stampMessage(%q, source %q) =\n%q\nwant\n%q", c.msg, c.source, got, c.want)
			}
		})
	}
}
