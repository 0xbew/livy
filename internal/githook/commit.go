package githook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/0xbew/livy/internal/extract/claudecode"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/ticket"
)

// `livy git post-commit` records each commit made in an in-scope repo as commit.recorded:
// its SHA and, when an agent's shell made it, the harness, the session and the session's
// newest prompt, which is what joins a commit to the prompts behind it. `livy git
// post-rewrite <amend|rebase>` records git's old -> new SHA map as commit.rewritten, one
// line per pair, so a commit stays traceable after an amend or a rebase.
//
// Every commit is recorded, the picks a rebase replays included: git runs post-commit for each,
// and its state cannot tell a replayed pick from new work committed at a break, edit or
// conflict stop. A reader takes a commit that is the new side of a rebase commit.rewritten line
// as a replay.

// The rewrite kinds git passes post-rewrite, as the schema names them.
var rewriteKinds = []string{"amend", "rebase"}

// objectName is a full SHA-1 or SHA-256 object name.
var objectName = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// PostCommit handles one post-commit call and returns the exit code, always 0.
func PostCommit(_ []string) (code int) {
	rec := hook.NewRecorder(io.Discard)
	defer recoverInto(rec, "post-commit", &code)
	if !inScope(rec, "commit not recorded") {
		return 0
	}
	sha, err := git(hookTimeout(), "rev-parse", "HEAD")
	if err != nil || !objectName.MatchString(sha) {
		rec.Error(fmt.Sprintf("post-commit: could not read HEAD (%v); commit not recorded", err), true)
		return 0
	}
	e := rec.Event("commit.recorded")
	e["commit_sha"] = sha
	e["ticket_key"] = commitTicket(rec, sha, e["branch"])
	if a, ok := committer(os.Getenv); ok {
		e["harness"], e["session_id"] = a.harness, a.session
		switch id := lastPrompt(a); {
		case id != "":
			e["prompt_id"] = id
		default:
			rec.Error("post-commit: no prompt id found for session "+a.session+"; commit "+sha+" recorded without one", true)
		}
	}
	if !rec.Append(e) {
		rec.Error("could not write event commit.recorded", true)
	}
	return 0
}

// commitTicket is the commit's ticket key: the branch's, else its message subject's, else
// ticket.None. Only the subject: a body's version tokens (SHA-256, UTF-8) read as keys. A ticket.regex that cannot be used, or a message git cannot give, is said.
func commitTicket(rec *hook.Recorder, sha string, branch any) string {
	m, problem := ticket.FromSettings(hook.Settings())
	if problem != "" {
		rec.Error("post-commit: "+problem, true)
	}
	b, _ := branch.(string)
	msg, err := git(hookTimeout(), "log", "-1", "--format=%s", sha, "--")
	if err != nil {
		rec.Error(fmt.Sprintf("post-commit: could not read the message of %s (%v); its ticket key is the branch's only", sha, err), true)
	}
	return m.Key(b, msg)
}

// PostRewrite handles one post-rewrite call and returns the exit code, always 0. Git passes
// the kind of rewrite as the argument and one "<old> <new> [<extra>]" line per rewritten
// commit on stdin.
func PostRewrite(args []string, stdin *os.File) (code int) {
	rec := hook.NewRecorder(io.Discard)
	defer recoverInto(rec, "post-rewrite", &code)
	if len(args) != 1 || !slices.Contains(rewriteKinds, args[0]) {
		return 0 // not git calling
	}
	if !inScope(rec, "rewrite not recorded") {
		return 0
	}
	a, agentCommitting := committer(os.Getenv)
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || !objectName.MatchString(f[0]) || !objectName.MatchString(f[1]) {
			rec.Error(fmt.Sprintf("post-rewrite: a %s line is not \"<old> <new>\" (%d bytes); that rewrite not recorded", args[0], len(sc.Bytes())), true)
			continue
		}
		e := rec.Event("commit.rewritten")
		e["old_commit_sha"], e["commit_sha"], e["rewrite_kind"] = f[0], f[1], args[0]
		if agentCommitting {
			e["harness"], e["session_id"] = a.harness, a.session
		}
		if !rec.Append(e) {
			rec.Error("could not write event commit.rewritten", true)
		}
	}
	if err := sc.Err(); err != nil {
		rec.Error("post-rewrite: could not read git's list ("+err.Error()+"); later rewrites not recorded", true)
	}
	return 0
}

// recoverInto turns a panic into exit 0 and a ledger.error naming the hook.
func recoverInto(rec *hook.Recorder, name string, code *int) {
	if p := recover(); p != nil {
		*code = 0
		defer func() { _ = recover() }() // the error line may hit the same panic
		rec.Error(fmt.Sprintf("%s: panic (%v); not recorded", name, p), true)
	}
}

// inScope is whether the repo is in scope; a git timeout deciding it is said, naming what was lost.
func inScope(rec *hook.Recorder, lost string) bool {
	if rec.InScope() {
		return true
	}
	if _, timedOut := rec.GitTimeout(); timedOut {
		rec.GitTimedOutError("", lost)
	}
	return false
}

// lastPrompt is the committing session's newest prompt id: the newest prompt.submitted line
// the ledger still holds for it, else (Claude Code) the newest prompt in its transcript, which
// outlives the ledger's rotation, or (Codex) the main thread's newest turn in its rollout. ""
// when neither names one.
func lastPrompt(a agent) string {
	if id := ledgerPrompt(a.session); id != "" {
		return id
	}
	if a.harness == harnessCodex {
		return codexPrompt(a.session)
	}
	if a.harness != harnessClaudeCode {
		return ""
	}
	mains, _ := filepath.Glob(filepath.Join(claudecode.LiveRoot(), "*", a.session+".jsonl"))
	for _, main := range mains {
		if id := transcriptPrompt(main); id != "" {
			return id
		}
	}
	return ""
}

// promptLine is the part of a ledger line that names a submitted prompt.
type promptLine struct {
	Event     string `json:"event"`
	SessionID string `json:"session_id"`
	PromptID  string `json:"prompt_id"`
}

// ledgerPrompt is the prompt id of the session's newest prompt.submitted line in the ledger.
func ledgerPrompt(session string) (id string) {
	f, err := os.Open(filepath.Join(hook.DataDir(), ledger.FileName))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }() // opened read-only: a close error loses nothing
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		var l promptLine
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.Event == "prompt.submitted" && l.SessionID == session && l.PromptID != "" {
			id = l.PromptID
		}
	}
	if sc.Err() != nil {
		return "" // a line too long to read may be newer: the transcript decides
	}
	return id
}

// transcriptRecord is the part of a transcript record that names its prompt. Claude Code puts
// promptId on user records only: the prompt and the tool results that follow it.
type transcriptRecord struct {
	Type     string `json:"type"`
	PromptID string `json:"promptId"`
}

// transcriptPrompt is the promptId of the newest user record in the transcript's last
// transcriptTail bytes.
func transcriptPrompt(path string) string {
	buf, start := tail(path)
	lines := bytes.Split(buf, []byte("\n"))
	for i, line := range slices.Backward(lines) {
		if start > 0 && i == 0 {
			break // the first line read may start mid-line
		}
		if !bytes.Contains(line, []byte(`"promptId"`)) {
			continue
		}
		var r transcriptRecord
		if json.Unmarshal(line, &r) == nil && r.Type == "user" && r.PromptID != "" {
			return r.PromptID
		}
	}
	return ""
}
