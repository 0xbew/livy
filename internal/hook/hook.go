// Package hook is `livy hook <event> [key=value ...]`: one ledger line per Claude Code
// hook event, with the payload read from stdin under a deadline. It never blocks (every
// path exits 0), never fails silently (a line it cannot write becomes a ledger.error),
// makes no network call, and writes nothing outside an in-scope repository: hooks are
// installed per machine and fire in personal projects too, and a line saying "skipped
// repo X" would itself record personal activity.
//
// Two things are deliberately never recorded: a PR number parsed from `gh pr create`
// output (unverified; the branch is recorded instead), and the previous branch (wrong
// exactly when it matters; the ledger's timeline already has it).
package hook

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/0xbew/livy/internal/buildinfo"
	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/config"
	"github.com/0xbew/livy/internal/event"
	"github.com/0xbew/livy/internal/gitinfo"
	"github.com/0xbew/livy/internal/keyspace"
	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/ledgerjson"
	"github.com/0xbew/livy/internal/oldnames"
	"github.com/0xbew/livy/internal/pytext"
	"github.com/0xbew/livy/internal/ticket"
)

const schemaVersion = 1

// Events is the closed set. An unknown name becomes a ledger.error naming it, never a row.
var Events = []string{"work.started", "work.pr_opened", "session.started", "session.ended", "turn.ended",
	"model.switched", "instructions.loaded", "ledger.error", "prompt.submitted", "usage.api_call",
	"work.pr_linked", "interaction", "call.detail", "check.decision", "tool.outcome", "tool.call",
	"agent.launched", "prompt.captured", "doctor.status", skillLoadedEvent}

// lift maps a payload key to the field it fills, first one wins. Everything else on stdin
// (transcript_path and cwd among it) is never copied.
var lift = [][2]string{{"session_id", "session_id"}, {"prompt_id", "prompt_id"}, {"model", "model"},
	{"from_model", "from_model"}, {"to_model", "to_model"}, {"reason", "reason"}, {"source", "reason"},
	{"load_reason", "reason"}, {"tool_name", "tool"}, {"hook_event_name", "hook"}}

// reserved are never taken from an override: event would bypass the closed set, ts and
// schema_version would forge when and under which contract a line was written.
var reserved = map[string]bool{"event": true, "ts": true, "schema_version": true, "raw_stdin_bytes": true}

// overridable are the fields a key=value override may set; any other would fail the schema.
var overridable = map[string]bool{"repo": true, "branch": true, "session_id": true, "prompt_id": true,
	"model": true, "from_model": true, "to_model": true, "reason": true, "instruction_files": true,
	"tool": true, "hook": true}

// run is one hook invocation's state.
type run struct {
	stderr  io.Writer
	harness string // "" for Claude Code, harnessCodex for Codex's hooks
	git     *gitinfo.Git
	path    string // the ledger file
	size    int    // stdin bytes, carried on every error line
}

// Run handles one hook invocation and returns the exit code, which is always 0.
func Run(args []string, stdin *os.File, stderr io.Writer) (code int) {
	var r *run
	defer func() {
		if p := recover(); p != nil {
			code = 0
			defer func() { _ = recover() }() // the error line may hit the same panic
			if r != nil {
				r.errorEvent(fmt.Sprintf("unhandled panic: %v", p), true)
			}
		}
	}()
	r = &run{stderr: stderr, git: gitinfo.New(stderr), path: filepath.Join(dataDir(), ledger.FileName)}
	r.git.Hosts = hosts(stderr)
	args, r.harness = harnessFlag(args)
	r.handle(args, stdin, true)
	return 0
}

// harnessFlag takes a leading `--harness <name>` (or `--harness=<name>`) off args.
func harnessFlag(args []string) (rest []string, harness string) {
	switch {
	case len(args) >= 2 && args[0] == "--harness":
		return args[2:], args[1]
	case len(args) >= 1 && strings.HasPrefix(args[0], "--harness="):
		return args[1:], strings.TrimPrefix(args[0], "--harness=")
	}
	return args, ""
}

func (r *run) inScope() bool {
	return r.git.InScope(r.git.Origin(""), func() []string { return scope(r.stderr) })
}

func (r *run) handle(args []string, stdin *os.File, enforceScope bool) {
	if len(args) > 0 && args[0] == "session.started" {
		r.managedRefused()
	}
	// Out of scope means this invocation leaves no trace at all, error lines included.
	if enforceScope && !r.inScope() && r.leaveNoTrace() {
		return
	}
	if len(args) == 0 {
		r.errorEvent("livy hook invoked with no event name", true)
		return
	}
	if r.harness != "" && r.harness != harnessCodex {
		// Another harness's payload would be read as the wrong shape.
		r.errorEvent("livy hook: --harness "+pytext.Repr(r.harness)+" is not codex; event not recorded", true)
		return
	}
	name := args[0]
	if name == "session.started" {
		for _, n := range oldNameNotes() {
			r.errorEvent("old "+oldnames.Livy+" name still in use: "+n, true)
		}
	}
	overrides := parseOverrides(args[1:])
	p := readStdin(stdin)
	r.size = p.size

	if !slices.Contains(Events, name) {
		r.errorEvent("unknown event name: "+pytext.Repr(name), true)
		return
	}
	if r.harness == harnessCodex {
		r.codexEvent(name, overrides, p)
		return
	}
	if name == "work.pr_opened" && !r.verifiedPrCreate(p) {
		return
	}
	if ignored := ignoredOverrides(overrides); len(ignored) > 0 {
		r.errorEvent(fmt.Sprintf("ignored override(s) %s (event %s still recorded)", strings.Join(ignored, ", "), name), true)
	}
	if name == skillLoadedEvent {
		r.recordSkill(overrides, p)
		return
	}
	// Malformed stdin: both lines. The error keeps the failure visible; the event keeps
	// the fact that it happened.
	if p.problem != "" {
		r.errorEvent(fmt.Sprintf("%s (event %s still recorded)", p.problem, name), true)
	}
	rec := r.build(name, overrides, p)
	if !r.append(rec) {
		r.errorEvent("could not write event "+name, true)
	}
}

// managedRefused writes a ledger.error for a refused managed file, once per session and
// before the scope check: while it is refused no repository is in scope, so every later
// event of the session leaves no trace and this line is the only one saying why.
func (r *run) managedRefused() {
	res := settings()
	if !res.ManagedUnusable {
		return
	}
	for _, p := range res.Refused {
		if p.Layer == config.Managed {
			r.errorLine("config: "+p.String(), false, false)
		}
	}
}

// leaveNoTrace is true when an out-of-scope invocation stops here: always, unless the
// folder is in a category this hook still records. A git timeout is said either way.
func (r *run) leaveNoTrace() bool {
	if r.git.TimedOut() {
		r.timeoutError()
		return true
	}
	cwd, _ := gitinfo.Cwd()
	if r.git.Category(cwd) != "" {
		return false
	}
	if r.git.TimedOut() {
		r.timeoutError()
	}
	return true
}

// parseOverrides is the key=value arguments after the event name, blank keys dropped.
func parseOverrides(args []string) map[string]string {
	overrides := map[string]string{}
	for _, tok := range args {
		if k, v, ok := strings.Cut(tok, "="); ok {
			if k = pytext.Strip(k); k != "" {
				overrides[k] = v
			}
		}
	}
	return overrides
}

// ignoredOverrides is the override keys that may not be set, sorted.
func ignoredOverrides(overrides map[string]string) []string {
	var ignored []string
	for k := range overrides {
		if reserved[k] || !overridable[k] {
			ignored = append(ignored, k)
		}
	}
	sort.Strings(ignored)
	return ignored
}

// verifiedPrCreate is whether work.pr_opened is written: only for a verified `gh pr
// create`. An event with no readable command is not evidence of a PR, so it becomes an
// error instead.
func (r *run) verifiedPrCreate(p payload) bool {
	input, _ := p.obj["tool_input"].(map[string]any)
	command, ok := input["command"].(string)
	if len(p.obj) == 0 || !ok {
		why := p.problem
		if why == "" {
			why = "no tool_input.command in the payload"
		}
		r.errorEvent(fmt.Sprintf("work.pr_opened not recorded: cannot verify the command (%s)", why), true)
		return false
	}
	return RunsGhPrCreate(command)
}

func (r *run) base(name string) map[string]any {
	return map[string]any{"schema_version": int64(schemaVersion), "ts": clock.Seconds(clock.Now()), "event": name}
}

func (r *run) build(name string, overrides map[string]string, p payload) map[string]any {
	rec := r.base(name)
	rec["repo"] = r.git.Slug()
	rec["branch"] = r.branch(name)
	if b, ok := buildinfo.Read(); ok {
		rec["recorder_build"] = b.Rev
	}
	if p.size > 0 {
		rec["raw_stdin_bytes"] = int64(p.size)
	}
	for _, kv := range lift {
		if s, ok := p.obj[kv[0]].(string); ok && pytext.Strip(s) != "" {
			if _, set := rec[kv[1]]; !set {
				rec[kv[1]] = pytext.Strip(s)
			}
		}
	}
	if p.obj != nil {
		r.instructionFiles(rec, name, p.obj)
	}
	applyOverrides(rec, overrides)
	if name == "turn.ended" && p.obj != nil {
		handback(rec, p.obj)
	}
	if name == "session.started" {
		r.sessionTags(rec)
	}
	if name == "work.started" {
		r.ticketKey(rec)
	}
	if name == "work.pr_opened" {
		r.target(rec, p.obj)
	}
	return rec
}

// ticketKey adds the ticket key the branch names, or ticket.None. A ticket.regex that cannot
// be used is said, and the default pattern decides.
func (r *run) ticketKey(rec map[string]any) {
	m, problem := ticket.FromSettings(Settings())
	if problem != "" {
		r.errorEvent(problem, true)
	}
	branch, _ := rec["branch"].(string)
	rec["ticket_key"] = m.Key(branch)
}

// instructionFiles is the files that actually loaded, an observed fact (InstructionsLoaded
// fires once per file with a singular file_path; the list form is for manual invocation).
func (r *run) instructionFiles(rec map[string]any, name string, obj map[string]any) {
	files := obj["instruction_files"]
	if !pytext.Truthy(files) {
		files = obj["files"]
	}
	if list, ok := files.([]any); ok {
		var paths []any
		for _, f := range list {
			if s, ok := f.(string); ok && pytext.Strip(s) != "" {
				paths = append(paths, r.displayPath(pytext.Strip(s)))
			}
		}
		if len(paths) > 0 {
			rec["instruction_files"] = paths
		}
	}
	if s, ok := obj["file_path"].(string); ok && name == "instructions.loaded" && pytext.Strip(s) != "" {
		if _, set := rec["instruction_files"]; !set {
			rec["instruction_files"] = []any{r.displayPath(pytext.Strip(s))}
		}
	}
}

// applyOverrides sets each allowed key=value: an explicit one wins over anything lifted
// from the payload.
func applyOverrides(rec map[string]any, overrides map[string]string) {
	for k, v := range overrides {
		if reserved[k] || !overridable[k] {
			continue
		}
		if k != "instruction_files" {
			rec[k] = v
			continue
		}
		parts := []any{}
		for s := range strings.SplitSeq(v, ",") {
			if s != "" {
				parts = append(parts, s)
			}
		}
		rec[k] = parts
	}
}

// handback is what the Stop payload says about who the turn was handed back to: counts
// and a flag, never the reply text.
func handback(rec, obj map[string]any) {
	for _, kv := range [][2]string{{"background_tasks", "background_task_count"}, {"session_crons", "session_cron_count"}} {
		if list, ok := obj[kv[0]].([]any); ok {
			rec[kv[1]] = int64(len(list))
		}
	}
	if b, ok := obj["stop_hook_active"].(bool); ok {
		rec["stop_hook_active"] = b
	}
}

var (
	roles     = roleSet()
	roleToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)
	sessionID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	agentID   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// roleSet is the agent_role values recorded as themselves: the public ones and the private
// build's own.
func roleSet() map[string]bool {
	set := map[string]bool{"orchestrator": true, "review": true, "librarian": true, "labeller": true}
	for _, r := range oldnames.Roles {
		set[r] = true
	}
	return set
}

// Launcher tags: the variables a launcher sets when it starts a top-level session. Each is
// the Livy name, then the Python tool's name, read only while the Livy one is unset or blank.
var (
	roleEnv          = [2]string{"LIVY_AGENT_ROLE", oldnames.LauncherEnv("AGENT_ROLE")}
	parentSessionEnv = [2]string{"LIVY_PARENT_SESSION", oldnames.LauncherEnv("PARENT_SESSION")}
	parentAgentEnv   = [2]string{"LIVY_PARENT_AGENT", oldnames.LauncherEnv("PARENT_AGENT")}
)

// launcherTag is the first of names with a non-blank value, and that value; with none, the
// Livy name and "".
func launcherTag(names [2]string) (string, string) {
	for _, n := range names {
		if v := pytext.Strip(oldnames.Getenv(n)); v != "" {
			return n, v
		}
	}
	return names[0], ""
}

// sessionTags adds the launcher's role and parent tags, each checked; a refused one is said,
// naming the variable it was read from. A role is never guessed: no tag, no field. An
// unknown role is `other`.
func (r *run) sessionTags(rec map[string]any) {
	roleName, role := launcherTag(roleEnv)
	parentName, parent := launcherTag(parentSessionEnv)
	agentName, agent := launcherTag(parentAgentEnv)
	if role != "" {
		if roles[strings.ToLower(role)] {
			rec["agent_role"] = strings.ToLower(role)
		} else {
			rec["agent_role"] = "other"
			if roleToken.MatchString(role) {
				rec["agent_role_raw"] = role
			} else {
				r.errorEvent(roleName+" is not a plain token; recorded as role other, without its value", true)
			}
		}
	}
	session, _ := rec["session_id"].(string)
	switch {
	case parent != "" && !sessionID.MatchString(parent):
		r.errorEvent(parentName+" is not a session id; not recorded", true)
	case parent != "" && parent == session:
		r.errorEvent(parentName+" names this session itself; not recorded", true)
	case parent != "":
		rec["parent_session_id"] = parent
	}
	_, hasParent := rec["parent_session_id"]
	switch {
	case agent != "" && !agentID.MatchString(agent):
		r.errorEvent(agentName+" is not an agent id; not recorded", true)
	case agent != "" && !hasParent:
		if parent == "" {
			// nothing was read for the session: name the session variable of the agent's pair
			parentName = parentSessionEnv[0]
			if agentName == parentAgentEnv[1] {
				parentName = parentSessionEnv[1]
			}
		}
		r.errorEvent(agentName+" is set without a usable "+parentName+"; not recorded", true)
	case agent != "":
		rec["parent_agent_id"] = agent
	}
}

// displayPath is a loaded file's path without the engineer's home directory in it:
// repo-relative inside the repo, ~/-relative under home, else unchanged. The file itself
// is never resolved through a symlink (CLAUDE.md -> AGENTS.md loaded as CLAUDE.md); only
// its directory is, as a second try.
func (r *run) displayPath(path string) string {
	abs := path
	if !filepath.IsAbs(abs) {
		cwd, ok := gitinfo.Cwd()
		if !ok {
			return path
		}
		abs = pyJoin(cwd, abs)
	}
	abs = normpath(abs)
	candidates := []string{abs, pyJoin(realpath(filepath.Dir(abs)), filepath.Base(abs))}
	if top := r.git.Toplevel(); top != "" {
		if rel, ok := relativeTo(dedupe(top, realpath(top)), candidates, true); ok {
			return rel
		}
	}
	h := os.Getenv("HOME")
	if h == "" {
		h = home()
	}
	if rel, ok := relativeTo(dedupe(normpath(h), realpath(h)), candidates, false); ok {
		return "~/" + rel
	}
	return path
}

// relativeTo is the first candidate's path relative to the first root it lies under, roots
// first; orEqual also takes a candidate that is the root itself.
func relativeTo(roots, candidates []string, orEqual bool) (string, bool) {
	for _, root := range roots {
		for _, c := range candidates {
			if (orEqual && c == root) || strings.HasPrefix(c, root+"/") {
				if rel, err := filepath.Rel(root, c); err == nil {
					return rel, true
				}
			}
		}
	}
	return "", false
}

func dedupe(a, b string) []string {
	if a == b {
		return []string{a}
	}
	return []string{a, b}
}

// realpath is os.path.realpath: symlinks resolved as far as the path exists.
func realpath(p string) string {
	if p == "/" || p == "." || p == "" {
		return p
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return pyJoin(realpath(filepath.Dir(p)), filepath.Base(p))
}

// append validates rec against the schema, encodes it as Python does, and appends it. It
// refuses a line missing a required field: an empty or partial line looks like data.
func (r *run) append(rec map[string]any) bool {
	for _, k := range []string{"ts", "event", "repo"} {
		if s, _ := rec[k].(string); s == "" {
			return false
		}
	}
	raw, err := ledgerjson.Marshal(rec)
	if err != nil {
		return false
	}
	e, err := event.Decode(raw)
	if err != nil {
		return false
	}
	line, err := e.Encode()
	if err != nil {
		return false
	}
	done, err := ledger.Append(filepath.Dir(r.path), append(line, '\n'))
	switch {
	case errors.Is(err, ledger.ErrClose):
		fmt.Fprintf(r.stderr, "ledger: %v\n", err)
	case err != nil:
		return false
	}
	if err := keyspace.AddLedger(filepath.Dir(r.path), append(line, '\n')); err != nil {
		fmt.Fprintf(r.stderr, "%v\n", err) // the line is in the ledger; `livy reindex` picks it up
	}
	if done.Spilled {
		fmt.Fprintln(r.stderr, "ledger: lock not acquired; line kept in spill/ for the emitter")
	}
	// A ledger.error about the check is not checked again: the same failure would loop.
	if done.Unchecked != nil && rec["event"] != "ledger.error" {
		r.errorEvent(uncheckedReason(done.Unchecked, rec["event"]), false)
	}
	return true
}

// uncheckedReason is the ledger.error reason for a line written without the torn-line check.
func uncheckedReason(err error, event any) string {
	return fmt.Sprintf("ledger: the last line could not be checked for a torn write (%v); event %v written without it", err, event)
}

// errorEvent writes a ledger.error. Outside an in-scope repo the error is kept but the
// repo and branch are not: the failure matters, the name of a personal project does not.
func (r *run) errorEvent(reason string, askGit bool) { r.errorLine(reason, askGit, true) }

// errorLine is errorEvent; withSize false leaves raw_stdin_bytes off, as on the lines
// about a tool call's target, which describe the call rather than the payload.
func (r *run) errorLine(reason string, askGit, withSize bool) {
	scoped := askGit && r.inScope()
	rec := r.base("ledger.error")
	rec["repo"] = "unknown"
	rec["reason"] = reason
	if scoped {
		rec["repo"] = r.git.Slug()
		rec["branch"] = r.git.Branch()
	}
	if withSize && r.size > 0 {
		rec["raw_stdin_bytes"] = int64(r.size)
	}
	if !r.append(rec) {
		// Nowhere left to write it: say so where a person running the hook can see it.
		fmt.Fprintf(r.stderr, "ledger: could not write a ledger.error (%s)\n", reason)
	}
}

// branch is the branch for event name. When git ran out of time saying, a ledger.error says
// so: "unknown" alone reads as a folder outside a repo, and tokens and prompts join to PRs
// by branch. The error carries no repo, as asking git again would time out again.
func (r *run) branch(name string) string {
	b, why := r.git.BranchOrWhy()
	if why != "" {
		r.errorEvent(fmt.Sprintf("%s for event %s; its branch is unknown", why, name), false)
	}
	return b
}

// timeoutError names only the timeout: asking git for the repo again would time out again.
func (r *run) timeoutError() {
	r.errorEvent(fmt.Sprintf("git timed out after %ss; repo scope unconfirmed, event not recorded",
		gitinfo.FormatSeconds(r.git.Timeout())), false)
}
