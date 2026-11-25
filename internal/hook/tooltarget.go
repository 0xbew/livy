package hook

import (
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/0xbew/scrub"

	"github.com/0xbew/livy/internal/tooltarget"
)

// A tool call's target goes to the local tool-target store (package tooltarget), never the
// ledger: the file a read or edit named (repo-relative, or ~/ under home), the folder a search
// ran in, the program a shell command ran (with its subcommand for a few tools whose subcommand
// is the useful part: `go test`, `git push`), the host a fetch reached, a subagent's task label
// (the Agent call's description, never its prompt) and a skill's name. A search pattern, a
// command's arguments, a URL's path and query, and any output are never read.

var (
	fileTools   = map[string]string{"Read": "file_path", "Edit": "file_path", "MultiEdit": "file_path", "Write": "file_path", "NotebookEdit": "notebook_path"}
	folderTools = map[string]bool{"Grep": true, "Glob": true, "LS": true}
	// preambles are commands that only set up the one that follows; the target is that one.
	preambles = map[string]bool{"cd": true, "pushd": true, "popd": true, "export": true, "set": true, "unset": true,
		"source": true, ".": true, ":": true, "true": true}
	// wrappers run the command after them; timeout takes a duration first.
	wrappers = map[string]bool{"sudo": true, "env": true, "time": true, "nohup": true, "command": true, "exec": true,
		"nice": true, "timeout": true}
	// subcommanded are the programs whose first plain word names what they do.
	subcommanded = map[string]bool{"git": true, "go": true, "gh": true, "npm": true, "pnpm": true, "yarn": true,
		"cargo": true, "docker": true, "kubectl": true, "gcloud": true, "uv": true}
	subcommandRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	// programRE is a program's base name: anything else (a quoted command line, a variable) is not one.
	programRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	hostRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}$`)
	// gitValuedOpts take a value before git's subcommand; gitPlain (target.go) take none.
	gitValuedOpts = map[string]bool{"-C": true, "-c": true}
)

// keepTarget writes the call's scrubbed target to the local store, after deleting the store's
// expired days. Nothing is kept without a denylist to audit it with. A failure is a ledger.error
// that names neither the target nor a path: the ledger ships.
func (r *run) keepTarget(rec, obj map[string]any) {
	if tooltarget.Prune(dataDir(), r.started) != nil {
		r.errorEvent("tool.call: expired tool targets could not be deleted", true)
	}
	id, _ := rec["tool_use_id"].(string)
	tool, _ := rec["tool"].(string)
	session, _ := rec["session_id"].(string)
	input, _ := obj["tool_input"].(map[string]any)
	raw := r.targetOf(tool, input)
	if id == "" || raw == "" {
		return
	}
	d, err := scrub.LoadDenylist(denylistFile())
	if err != nil {
		return // doctor's denylist row says there is none; without it no target is kept
	}
	target, err := tooltarget.Clean(raw, d)
	if err != nil || target == "" {
		return // refused by the audit: dropped, as a target is never needed
	}
	rec2 := tooltarget.Record{TS: r.started, SessionID: session, ToolUseID: id, Tool: tool, Target: target}
	if tooltarget.Append(dataDir(), rec2) != nil {
		r.errorEvent("tool.call: the call's target could not be written to the local tool-target store", true)
	}
}

// denylistFile is the denylist's local copy: denylist.path when set, else in the data folder.
func denylistFile() string {
	if v, ok := settings().Settings["denylist.path"].Value.(string); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return filepath.Join(dataDir(), "denylist.json")
}

// targetOf is a call's raw target, before scrubbing; "" for a tool with none.
func (r *run) targetOf(tool string, input map[string]any) string {
	str := func(k string) string { s, _ := input[k].(string); return strings.TrimSpace(s) }
	if key, ok := fileTools[tool]; ok && str(key) != "" {
		return r.displayPath(str(key))
	}
	switch {
	case folderTools[tool] && str("path") != "":
		return r.displayPath(str("path"))
	case tool == "Bash":
		return commandName(str("command"))
	case tool == "WebFetch":
		return hostOf(str("url"))
	case tool == "Agent" || tool == "Task":
		return str("description")
	case tool == "Skill":
		return str("skill")
	}
	return ""
}

// commandName is the program the command runs after its preambles (cd, export, assignments)
// and wrappers (sudo, env, timeout), by its base name, with its subcommand for a subcommanded
// program; "" when the shell alone could say (a variable or substitution in the program word).
func commandName(command string) string {
	name, found := "", false
	op := commands(command, func(words []word, _ string) bool {
		ws := programWords(words)
		if len(ws) == 0 || preambles[ws[0]] {
			return true
		}
		name, found = nameOf(ws), true
		return false
	})
	if op != nil && !found {
		return ""
	}
	return name
}

// programWords is a simple command's words from its program on: leading assignments and
// wrappers (an env's assignments and a timeout's duration with them) dropped. A wrapper given
// an option is the program itself: which word its option takes as a value, only it knows.
func programWords(words []word) []string {
	ws := make([]string, 0, len(words))
	for _, w := range words {
		ws = append(ws, w.text)
	}
	for len(ws) > 0 {
		switch {
		case assignRE.MatchString(ws[0]):
			ws = ws[1:]
		case wrappers[ws[0]]:
			wrapper := ws[0]
			ws = ws[1:]
			if wrapper == "timeout" && len(ws) > 0 && startsWithDigit(ws[0]) {
				ws = ws[1:]
			}
			if len(ws) > 0 && strings.HasPrefix(ws[0], "-") {
				return []string{wrapper}
			}
		default:
			return ws
		}
	}
	return ws
}

func startsWithDigit(s string) bool { return s != "" && s[0] >= '0' && s[0] <= '9' }

// nameOf is the program's base name, and its subcommand when the word right after it is one (for
// git, after its -C and -c pairs and its plain options). An option before the subcommand may take
// a value, so then the program alone is kept.
func nameOf(ws []string) string {
	base := path.Base(ws[0])
	if !programRE.MatchString(base) {
		return ""
	}
	rest := ws[1:]
	for base == "git" && len(rest) > 0 {
		switch {
		case gitPlain[rest[0]]:
			rest = rest[1:]
		case gitValuedOpts[rest[0]] && len(rest) > 1:
			rest = rest[2:]
		default:
			return gitName(rest)
		}
	}
	if subcommanded[base] && len(rest) > 0 && subcommandRE.MatchString(rest[0]) {
		return base + " " + rest[0]
	}
	return base
}

// gitName is git with its subcommand, when the first word after its options is one.
func gitName(rest []string) string {
	if subcommandRE.MatchString(rest[0]) {
		return "git " + rest[0]
	}
	return "git"
}

// hostOf is a URL's host name, without its scheme, credentials, port, path, query or fragment;
// "" when it names none. Read by hand: the hook imports nothing from net.
func hostOf(u string) string {
	_, rest, ok := strings.Cut(u, "://")
	if !ok {
		return ""
	}
	authority := rest
	if k := strings.IndexAny(rest, "/?#"); k >= 0 {
		authority = rest[:k]
		// An '@' past the authority may end credentials that hold a '/', '?' or '#': nothing in
		// front of it can be told from them, so such a URL gives no host.
		if strings.Contains(rest[k:], "@") {
			return ""
		}
	}
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		authority = authority[i+1:]
	}
	host, port, _ := strings.Cut(strings.ToLower(authority), ":")
	if strings.Trim(port, "0123456789") != "" || !hostRE.MatchString(host) {
		return ""
	}
	return host
}
