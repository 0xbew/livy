package hook

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/0xbew/livy/internal/gitinfo"
	"github.com/0xbew/livy/internal/pytext"
)

// Where one tool call worked: work.pr_opened carries target_repo, target_branch and
// target_via (cd | git_c | file_path | session), read from the folder the call itself
// named. The folder is held to resolve it and dropped; the command and path are never kept.
const (
	viaCD      = "cd"
	viaGit     = "git_c"
	viaFile    = "file_path"
	viaSession = "session"
	unknown    = "unknown"
	// unresolved is a leading cd or git -C whose folder only the shell knows.
	unresolved = ""
	// expand stands in for an unquoted `$`, so a quoted or escaped `$` stays literal text.
	expand = '\x00'
)

var (
	fileKeys  = []string{"file_path", "notebook_path"}
	pathTools = map[string]bool{"Grep": true, "Glob": true, "LS": true}
	cdFlags   = map[string]bool{"-L": true, "-P": true, "-e": true, "-@": true, "--": true}
	// Variables that change where cd or git -C ends up: a command setting one names no folder.
	steering = map[string]bool{"HOME": true, "CDPATH": true, "GIT_DIR": true, "GIT_WORK_TREE": true}
	// Between a preamble and the command it leads to: each runs only once the one before has.
	then       = map[string]bool{";": true, "&&": true, "\n": true}
	opChars    = ";&|()<>\n"
	pairs      = map[string]bool{"&&": true, "||": true, ";;": true, ">>": true, "<<": true, ">&": true, "<&": true, "&>": true, "|&": true}
	nameRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	assignRE   = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=`)
	variableRE = regexp.MustCompile("\x00" + `(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))`)
	gitPlain   = map[string]bool{"--no-pager": true}
	gitValued  = map[string]bool{"-c": true}
)

type word struct {
	text   string
	quoted int // index of the word's first quote, else its length (in characters)
}

// opaque is a word only the shell can read: an unclosed quote or a command substitution.
type opaque struct{ words []string }

// commands calls fn with each simple command's words and the operator that ends it ("" at
// the end), lazily; fn returns false to stop. A `(` is skipped. An opaque word stops the
// read and returns the words of the command it was met in.
func commands(command string, fn func([]word, string) bool) *opaque {
	lx := &lexer{r: []rune(command)}
	n := len(lx.r)
	var words []word
	for lx.i < n {
		c := lx.r[lx.i]
		switch {
		case isBlank(c):
			lx.i++
		case c == '\\' && lx.i+1 < n && lx.r[lx.i+1] == '\n':
			lx.i += 2
		case c == '#':
			lx.skipComment()
		case isOp(c):
			op := lx.op()
			if op == "(" {
				continue
			}
			if !fn(words, op) {
				return nil
			}
			words = nil
		default:
			w, ok := lx.word()
			if !ok {
				return &opaque{commandWords(words)}
			}
			words = append(words, w)
		}
	}
	fn(words, "")
	return nil
}

// lexer reads a command's runes from i.
type lexer struct {
	r []rune
	i int
}

func isBlank(c rune) bool { return c == ' ' || c == '\t' || c == '\r' }

func isOp(c rune) bool { return strings.ContainsRune(opChars, c) }

func (lx *lexer) skipComment() {
	for lx.i < len(lx.r) && lx.r[lx.i] != '\n' {
		lx.i++
	}
}

// op reads one operator, two characters when they pair.
func (lx *lexer) op() string {
	j := lx.i + 1
	if lx.i+2 <= len(lx.r) && pairs[string(lx.r[lx.i:lx.i+2])] {
		j = lx.i + 2
	}
	op := string(lx.r[lx.i:j])
	lx.i = j
	return op
}

// word reads one word; false when it is opaque.
func (lx *lexer) word() (word, bool) {
	n := len(lx.r)
	var w []rune
	quoted := -1
	for lx.i < n && !isBlank(lx.r[lx.i]) && !isOp(lx.r[lx.i]) {
		c := lx.r[lx.i]
		if (c == '\'' || c == '"') && quoted < 0 {
			quoted = len(w)
		}
		ok := true
		switch c {
		case '\'':
			w, ok = lx.singleQuoted(w)
		case '"':
			w, ok = lx.doubleQuoted(w)
		case '\\':
			if lx.i+1 < n {
				w = append(w, lx.r[lx.i+1])
			}
			lx.i += 2
		default:
			w, ok = lx.char(w, lx.i)
			lx.i++
		}
		if !ok {
			return word{}, false
		}
	}
	if quoted < 0 {
		quoted = len(w)
	}
	return word{string(w), quoted}, true
}

// char appends the rune at k, an unquoted `$` as expand; false at a command substitution.
func (lx *lexer) char(w []rune, k int) ([]rune, bool) {
	r := lx.r
	if r[k] == '`' || (r[k] == '$' && k+1 < len(r) && r[k+1] == '(') {
		return w, false
	}
	if r[k] == '$' {
		return append(w, expand), true
	}
	return append(w, r[k]), true
}

// singleQuoted appends the text up to the closing quote; false when it never closes.
func (lx *lexer) singleQuoted(w []rune) ([]rune, bool) {
	end := -1
	for k := lx.i + 1; k < len(lx.r); k++ {
		if lx.r[k] == '\'' {
			end = k
			break
		}
	}
	if end < 0 {
		return w, false
	}
	w = append(w, lx.r[lx.i+1:end]...)
	lx.i = end + 1
	return w, true
}

// doubleQuoted appends the text up to the closing quote, with `$` still expanding; false
// when it never closes or holds a command substitution.
func (lx *lexer) doubleQuoted(w []rune) ([]rune, bool) {
	n := len(lx.r)
	lx.i++
	for lx.i < n && lx.r[lx.i] != '"' {
		if lx.r[lx.i] == '\\' && lx.i+1 < n && strings.ContainsRune("\"\\$`", lx.r[lx.i+1]) {
			w = append(w, lx.r[lx.i+1])
			lx.i += 2
			continue
		}
		var ok bool
		if w, ok = lx.char(w, lx.i); !ok {
			return w, false
		}
		lx.i++
	}
	if lx.i >= n {
		return w, false
	}
	lx.i++
	return w, true
}

// assignment is one NAME=value; value nil when it is a quoted `~`, which the shell keeps.
type assignment struct {
	name  string
	value *string
}

// assignments is the (name, value) list of a command that only sets variables (`S=x`,
// `export S=x`), or ok false.
func assignments(words []word) ([]assignment, bool) {
	exporting := len(words) > 0 && words[0] == word{"export", len("export")}
	if exporting {
		words = words[1:]
	}
	var out []assignment
	for _, w := range words {
		m := assignRE.FindStringSubmatch(w.text)
		if exporting && m == nil && nameRE.MatchString(w.text) {
			continue // `export S` sets nothing
		}
		end := 0
		if m != nil {
			end = len([]rune(m[0]))
		}
		if m == nil || end > w.quoted {
			return nil, false
		}
		v := string([]rune(w.text)[end:])
		if strings.HasPrefix(v, "~") && w.quoted == end {
			out = append(out, assignment{m[1], nil})
		} else {
			out = append(out, assignment{m[1], &v})
		}
	}
	return out, len(words) > 0
}

// commandWords is a command's words after its prefix assignments (`S=x cd $S`), or none
// when a prefix sets a variable that moves cd or git.
func commandWords(words []word) []string {
	moved := false
	for len(words) > 0 {
		a, ok := assignments(words[:1])
		if !ok || len(a) == 0 {
			break
		}
		moved = moved || steering[strings.SplitN(words[0].text, "=", 2)[0]]
		words = words[1:]
	}
	if moved {
		return []string{}
	}
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = w.text
	}
	return out
}

// expandWord fills in each $NAME the command set to a literal; ok false when one is not.
func expandWord(w string, vars map[string]*string) (string, bool) {
	known := true
	out := variableRE.ReplaceAllStringFunc(w, func(m string) string {
		sub := variableRE.FindStringSubmatch(m)
		name := sub[1] + sub[2]
		v, ok := vars[name]
		if !ok || v == nil {
			known = false
			return ""
		}
		return *v
	})
	return out, known && !strings.ContainsRune(out, expand)
}

// pyJoin is os.path.join of two parts.
func pyJoin(a, b string) string {
	if strings.HasPrefix(b, "/") || a == "" {
		return b
	}
	if strings.HasSuffix(a, "/") {
		return a + b
	}
	return a + "/" + b
}

// gitDirectory is the folder `git -C <dir> ...` works in: unresolved when only the shell
// knows it, ok false when there is none.
func gitDirectory(words []string, vars map[string]*string) (string, bool) {
	folder, have := "", false
	for i := 0; i < len(words); {
		w := words[i]
		switch {
		case w == "-C" && i+1 < len(words):
			part, ok := expandWord(words[i+1], vars)
			if !ok {
				return unresolved, true
			}
			// Later relative -C folders are read from the one before; an empty one changes nothing.
			switch {
			case folder != "" && part != "":
				folder = pyJoin(folder, part)
			case part != "":
				folder = part
			}
			have = have || folder != ""
			i += 2
		case gitValued[w]:
			i += 2
		case gitPlain[w]:
			i++
		case strings.HasPrefix(w, "-"):
			return "", false
		default:
			return folder, have
		}
	}
	return folder, have
}

// LeadingDirectory is (via, folder) a command works in from its first command on: a cd or
// a git -C, after nothing but variable assignments, whose literal values fill in the folder.
func LeadingDirectory(command string) (via, folder string, ok bool) {
	vars := map[string]*string{}
	done := false
	var result struct {
		via, folder string
		ok          bool
	}
	stop := func(v, f string, k bool) bool {
		result.via, result.folder, result.ok, done = v, f, k, true
		return false
	}
	op := commands(command, func(words []word, operator string) bool {
		if len(words) == 0 {
			if operator == "\n" || operator == "" {
				return true // a blank line; a bare `;` or `&&` is a syntax error
			}
			return stop("", "", false)
		}
		pairs, isAssign := assignments(words)
		if !isAssign {
			v, f, k := firstCommand(commandWords(words), vars, true)
			return stop(v, f, k)
		}
		if !then[operator] {
			return stop("", "", false)
		}
		for _, p := range pairs {
			if steering[p.name] {
				return stop("", "", false)
			}
		}
		assign(vars, pairs, words[0].text == "export")
		return true
	})
	if op != nil && !done {
		return firstCommand(op.words, vars, false)
	}
	return result.via, result.folder, result.ok
}

// assign sets each pair's literal value in vars, or nil when its value is not literal.
// export expands all its words before it sets any; plain assignments go left to right.
func assign(vars map[string]*string, pairs []assignment, export bool) {
	before := map[string]*string{}
	maps.Copy(before, vars)
	for _, p := range pairs {
		if p.value == nil {
			vars[p.name] = nil
			continue
		}
		src := vars
		if export {
			src = before
		}
		if v, ok := expandWord(*p.value, src); ok {
			vars[p.name] = &v
		} else {
			vars[p.name] = nil
		}
	}
}

// firstCommand is (via, folder) for the first real command; whole is false when the word
// after the given ones was opaque.
func firstCommand(words []string, vars map[string]*string, whole bool) (string, string, bool) {
	if len(words) == 0 {
		return "", "", false
	}
	if words[0] == "git" {
		if !whole && words[len(words)-1] == "-C" {
			return viaGit, unresolved, true
		}
		f, ok := gitDirectory(words[1:], vars)
		return viaGit, f, ok
	}
	if words[0] != "cd" {
		return "", "", false
	}
	rest := words[1:]
	for len(rest) > 0 && cdFlags[rest[0]] {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		if whole {
			return viaCD, "~", true // a bare cd goes home
		}
		return viaCD, unresolved, true
	}
	if rest[0] == "-" {
		return viaCD, unresolved, true
	}
	f, ok := expandWord(rest[0], vars)
	if !ok {
		return viaCD, unresolved, true
	}
	return viaCD, f, true
}

// absolute is the absolute, normalised path, or ok false for a relative one with no cwd.
func absolute(path, cwd string) (string, bool) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		path = os.Getenv("HOME") + path[1:]
	}
	if !strings.HasPrefix(path, "/") {
		if !strings.HasPrefix(cwd, "/") {
			return "", false
		}
		path = pyJoin(cwd, path)
	}
	return normpath(path), true
}

// normpath is os.path.normpath, which keeps exactly two leading slashes.
func normpath(p string) string {
	if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
		return "/" + filepath.Clean(p[1:])
	}
	return filepath.Clean(p)
}

// NamedDirectory is (via, absolute folder) a tool call names, or ok false.
func NamedDirectory(tool string, input any, cwd string) (via, folder string, ok bool) {
	in, isMap := input.(map[string]any)
	if !isMap {
		return "", "", false
	}
	if tool == "Bash" {
		return bashDirectory(in, cwd)
	}
	return fileDirectory(tool, in, cwd)
}

// bashDirectory is (via, absolute folder) a shell command starts in, or ok false.
func bashDirectory(in map[string]any, cwd string) (string, string, bool) {
	command, isStr := in["command"].(string)
	if !isStr {
		return "", "", false
	}
	v, w, found := LeadingDirectory(command)
	if !found {
		return "", "", false
	}
	if w == "" {
		return v, unresolved, true
	}
	if abs, ok := absolute(w, cwd); ok {
		return v, abs, true
	}
	return v, unresolved, true
}

// fileDirectory is the folder of the file a tool call names, or the folder a path tool
// names, or ok false.
func fileDirectory(tool string, in map[string]any, cwd string) (string, string, bool) {
	for _, key := range fileKeys {
		if s, isStr := in[key].(string); isStr && pytext.Strip(s) != "" {
			if p, ok := absolute(pytext.Strip(s), cwd); ok {
				return viaFile, filepath.Dir(p), true
			}
			return "", "", false
		}
	}
	if pathTools[tool] {
		if s, isStr := in["path"].(string); isStr && pytext.Strip(s) != "" {
			if p, ok := absolute(pytext.Strip(s), cwd); ok {
				return viaFile, p, true
			}
		}
	}
	return "", "", false
}

// target adds where a work.pr_opened's call worked. Never fails the event: each problem is
// written as a ledger.error and the event is recorded, without a target if need be.
func (r *run) target(rec map[string]any, obj map[string]any) {
	sessionCwd, haveCwd := gitinfo.Cwd()
	cwd := sessionCwd
	if c, ok := obj["cwd"].(string); ok && strings.HasPrefix(c, "/") {
		cwd = c
	}
	tool, _ := obj["tool_name"].(string)
	via, folder, named := NamedDirectory(tool, obj["tool_input"], cwd)
	repo, _ := rec["repo"].(string)
	branch, _ := rec["branch"].(string)
	if branch == "" {
		branch = unknown
	}
	if !named {
		rec["target_via"], rec["target_repo"], rec["target_branch"] = viaSession, repo, branch
		return
	}
	var problems []string
	if folder != "" {
		if place := r.place(folder, sessionCwd, haveCwd, branch, &problems); place != nil {
			rec["target_via"], rec["target_repo"], rec["target_branch"] = via, place[0], place[1]
		} else {
			rec["target_via"] = via
		}
	} else {
		rec["target_via"] = via
	}
	for _, p := range problems {
		r.errorLine(fmt.Sprintf("%s (event %s still recorded)", p, rec["event"]), true, false)
	}
}

// root is the realpath of the nearest folder at or above folder that holds a .git, or "".
func root(folder string) string {
	for probe := folder; ; {
		if _, err := os.Stat(filepath.Join(probe, ".git")); err == nil {
			return realpath(probe)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return ""
		}
		probe = parent
	}
}

// place is (repo, branch) of an in-scope folder, or nil.
func (r *run) place(folder, sessionCwd string, haveCwd bool, sessionBranch string, problems *[]string) []string {
	top := root(folder)
	if top == "" {
		return nil
	}
	earlier := r.git.TimedOut()
	r.git.ResetTimedOut()
	url := r.git.Origin(top)
	if url == "" && r.git.BudgetSpent() {
		// Asked after the budget ran out, so git was never asked (a cached origin still answers).
		*problems = append(*problems, "target: git budget spent before the named folder's repo was read; "+
			"target_repo and target_branch not recorded")
		return nil
	}
	inScope := url != "" && r.git.InScope(url, func() []string { return orgs(r.stderr) })
	if r.git.TimedOut() {
		// A timeout is not "out of scope": say why the target is missing.
		*problems = append(*problems, "target: git timed out reading the named folder's repo; target_repo and "+
			"target_branch not recorded")
		return nil
	}
	if earlier {
		r.git.MarkTimedOut()
	}
	if !inScope {
		return nil
	}
	_, owner, name, ok := gitinfo.RemoteOwnerRepo(url)
	if !ok {
		return nil
	}
	slug := owner + "/" + name
	if _, err := os.Stat(folder); err != nil {
		// A worktree removed since: the repo is still known, which branch it held is not.
		return []string{slug, unknown}
	}
	if haveCwd && strings.HasPrefix(sessionCwd, "/") && root(sessionCwd) == top {
		return []string{slug, sessionBranch}
	}
	branch := r.git.Run("-C", top, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "" {
		*problems = append(*problems, "target: an in-scope folder's branch could not be read; recorded as unknown")
		branch = unknown
	}
	return []string{slug, branch}
}
