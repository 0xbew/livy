package githook

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/gitinfo"
	"github.com/0xbew/livy/internal/hook"
)

// `livy git prepare-commit-msg <msgfile> [source] [sha]` stamps a commit an agent makes
// with attribution trailers. Git hooks inherit the committing process's environment, so
// an agent's shell says which harness and session made the commit; a commit made by hand
// has neither and is left exactly as written.

// The harness names the Agent-Harness trailer carries.
const (
	harnessClaudeCode = "claude-code"
	harnessCodex      = "codex"
)

// modes are the Agent-Mode values the trailer check accepts; any other value of
// trailers.mode would split the axis it groups on.
var modes = []string{"drive", "assist", "manual"}

// agent is the harness making a commit and its session.
type agent struct{ harness, session string }

// sessionFacts are what Livy recorded about a session: its model and harness version, each
// "" when unknown.
type sessionFacts struct{ model, version string }

// PrepareCommitMsg handles one prepare-commit-msg call and returns the exit code, always 0:
// a commit is never blocked. A failure leaves the message as git wrote it and is recorded
// as a ledger.error.
func PrepareCommitMsg(args []string) (code int) {
	defer func() {
		if p := recover(); p != nil {
			code = 0
			defer func() { _ = recover() }() // the error line may hit the same panic
			record("ledger.error", fmt.Sprintf("reason=prepare-commit-msg: panic (%v); commit message left unchanged", p))
		}
	}()
	if len(args) < 1 || len(args) > 3 {
		return 0 // not git calling: git passes the message file, then at most a source and a sha
	}
	if _, ok := committer(os.Getenv); !ok {
		return 0 // a commit made by hand: nothing to read, nothing to write
	}
	source := ""
	if len(args) > 1 {
		source = args[1]
	}
	enabled, mode, problem := trailerSettings()
	if problem != "" {
		record("ledger.error", "reason=prepare-commit-msg: "+problem)
	}
	if !enabled {
		return 0
	}
	if err := stampFile(args[0], source, mode); err != nil {
		record("ledger.error", "reason=prepare-commit-msg: "+err.Error()+"; commit message left unchanged")
	}
	return 0
}

// TrailersEnabled is trailers.enabled: on unless a config layer sets it to false.
func TrailersEnabled() bool {
	v, ok := hook.Settings().Settings["trailers.enabled"]
	return !ok || v.Value != false
}

// trailerSettings is trailers.enabled (true unless set to false) and trailers.mode ("" when
// unset or not a known mode, which problem then names).
func trailerSettings() (enabled bool, mode, problem string) {
	if !TrailersEnabled() {
		return false, "", ""
	}
	v, ok := hook.Settings().Settings["trailers.mode"]
	if !ok {
		return true, "", ""
	}
	m, _ := v.Value.(string)
	if !slices.Contains(modes, m) {
		return true, "", fmt.Sprintf("trailers.mode %v is not one of drive, assist, manual; Agent-Mode not stamped", v.Value)
	}
	return true, m, ""
}

// stampFile rewrites the message file with the trailers added, through one fixed temp name
// beside it, so a failure part-way leaves git's message whole.
func stampFile(path, source, mode string) error {
	msg, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("could not read the message file: %w", err)
	}
	facts := func(a agent) sessionFacts { return recordedFacts(a, source) }
	out, err := stampMessage(msg, os.Getenv, source, facts, mode)
	if err != nil || bytes.Equal(out, msg) {
		return err
	}
	tmp := path + ".livy-tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		_ = os.Remove(tmp) // best effort: the failure that matters is returned
		return fmt.Errorf("could not write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // best effort: the failure that matters is returned
		return fmt.Errorf("could not replace the message file: %w", err)
	}
	return nil
}

// stampMessage is msg with the committing agent's trailers added: none when no agent is
// committing, and none on a merge or squash message, which describes other commits.
func stampMessage(msg []byte, getenv func(string) string, source string, lookup func(agent) sessionFacts, mode string) ([]byte, error) {
	a, ok := committer(getenv)
	if !ok || source == "merge" || source == "squash" {
		return msg, nil
	}
	comment := commentPrefix()
	if !hasText(msg, comment) {
		return msg, nil
	}
	return addTrailers(msg, trailerLines(a, lookup(a), mode), comment)
}

// autoCommentChars are the characters git picks a comment character from under
// core.commentChar=auto.
const autoCommentChars = "#;@!$%^&|:"

// commentPrefix is what starts a comment line in this repository's commit messages: the last
// core.commentString or core.commentChar in config order (the two are one setting, and the
// last one set wins across levels), else "#".
func commentPrefix() string {
	out, err := gitRun(hookTimeout(), nil, "config", "--get-regexp", `^core\.comment(string|char)$`)
	prefix := "#"
	if err != nil {
		return prefix // exit 1: neither is set
	}
	for l := range strings.SplitSeq(string(out), "\n") {
		if _, v, ok := strings.Cut(l, " "); ok && strings.TrimSpace(v) != "" {
			prefix = strings.TrimSpace(v)
		}
	}
	return prefix
}

// hasText is whether the message says anything but git's comments: trailers alone would turn
// an empty message, which git aborts, into a commit. Under "auto" a line starting with any of
// git's candidates counts as a comment, which can only leave a message unstamped.
func hasText(msg []byte, comment string) bool {
	for l := range strings.SplitSeq(string(msg), "\n") {
		if strings.TrimSpace(l) != "" && !isComment(l, comment) {
			return true
		}
	}
	return false
}

// isComment is whether the non-blank line l is a comment under comment (see hasText for "auto").
func isComment(l, comment string) bool {
	if comment == "auto" {
		return strings.ContainsRune(autoCommentChars, rune(l[0]))
	}
	return strings.HasPrefix(l, comment)
}

// committer is the agent harness whose shell is making this commit. Both harnesses' variables
// at once means one launched the other (Codex inherits CLAUDECODE and CLAUDE_CODE_SESSION_ID
// from a Claude Code shell), and the environment cannot say which one is committing: no agent.
func committer(getenv func(string) string) (agent, bool) {
	claude := getenv("CLAUDECODE") == "1" && getenv("CLAUDE_CODE_SESSION_ID") != ""
	codex := getenv("CODEX_THREAD_ID")
	switch {
	case claude && codex != "":
		return agent{}, false
	case claude:
		return agent{harness: harnessClaudeCode, session: getenv("CLAUDE_CODE_SESSION_ID")}, true
	case codex != "":
		return agent{harness: harnessCodex, session: codex}, true
	}
	return agent{}, false
}

// trailerLines is the block in the org's order; a fact Livy did not record is left out.
func trailerLines(a agent, f sessionFacts, mode string) []string {
	var out []string
	if f.model != "" {
		out = append(out, "Agent-Model: "+f.model)
	}
	if f.version != "" {
		out = append(out, "Agent-Harness: "+a.harness+"@"+f.version)
	}
	out = append(out, "Agent-Session: "+a.session)
	if mode != "" {
		out = append(out, "Agent-Mode: "+mode)
	}
	return out
}

// addTrailers adds each trailer to the message's final trailer block, as `git commit
// --trailer` does (no divider), unless the block already has its key: an amend, or an agent
// that wrote its own, keeps what the message says. The keys are compared here, exactly:
// git's own --if-exists matches a key by prefix, so an existing Agent-Mode would suppress
// Agent-Model. An agent's Agent-* paragraph above the final block is joined to it first
// (joinAgentBlock), so its keys count; with nothing to add the message is left as written.
func addTrailers(msg []byte, lines []string, comment string) ([]byte, error) {
	joined := joinAgentBlock(msg, comment)
	existing, err := gitRun(hookTimeout(), bytes.NewReader(joined), "interpret-trailers", "--no-divider", "--parse")
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for l := range strings.SplitSeq(string(existing), "\n") {
		if k, _, ok := strings.Cut(l, ":"); ok {
			have[strings.ToLower(strings.TrimSpace(k))] = true
		}
	}
	var add []string
	for _, l := range lines {
		if k, _, _ := strings.Cut(l, ":"); !have[strings.ToLower(k)] {
			add = append(add, "--trailer", l)
		}
	}
	if len(add) == 0 {
		return msg, nil
	}
	args := append([]string{"interpret-trailers", "--no-divider", "--where", "end", "--if-exists", "add"}, add...)
	return gitRun(hookTimeout(), bytes.NewReader(joined), args...)
}

// hookTimeout is the git timeout the hooks share (gitinfo's setting).
func hookTimeout() time.Duration {
	return time.Duration(gitinfo.New(io.Discard).Timeout() * float64(time.Second))
}

// gitRun runs one git command under timeout with stdin; its untrimmed output.
func gitRun(timeout time.Duration, stdin io.Reader, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdin = stdin
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("git %s: no answer in %ss: %w", args[0], gitinfo.FormatSeconds(timeout.Seconds()), ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return out, nil
}
