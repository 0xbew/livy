package hook

import (
	"regexp"
	"strings"

	"github.com/0xbew/livy/internal/pytext"
)

var (
	envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	flagName      = regexp.MustCompile(`^--?[A-Za-z][\p{L}\p{N}_-]*$`)
	// Commands that run the next word as a command (`then`, `do`... start one in a compound).
	plainWrappers = map[string]bool{"command": true, "nohup": true, "time": true, "exec": true, "sudo": true,
		"then": true, "do": true, "else": true, "elif": true, "!": true, "{": true}
)

// RunsGhPrCreate reports whether some simple command in command IS `gh pr create`.
// work.pr_opened must not rest on the hook's `if` filter alone, which misfires; a false
// one invents a work boundary. The command is split as the shell splits it, so quoted text
// that only mentions the words (a commit message, a heredoc body) never counts, while a
// command substitution, which runs, does. The text is never kept.
func RunsGhPrCreate(command string) bool {
	for _, segment := range shellSegments(withoutHeredocBodies(command)) {
		if isGhPrCreate(stripWrappers(shellWords(segment))) {
			return true
		}
	}
	return false
}

// shellSegments splits at each simple-command boundary outside quotes: newline ; & | ),
// the `(` that opens a subshell, and the `$(` or backtick that opens a substitution, which
// runs a command even inside double quotes. A stack of contexts restores the enclosing
// double quote when a substitution closes, and not when a subshell inside it does. Comments and line continuations are dropped.
func shellSegments(s string) []string {
	sc := &segScan{r: []rune(s), stack: []int{segCommand}}
	r := sc.r
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case sc.single:
			sc.single = c != '\''
			sc.cur = append(sc.cur, c)
		case c == '\\' && i+1 < len(r) && r[i+1] == '\n':
			i++ // a line continuation joins the lines
		case c == '\\':
			sc.cur = append(sc.cur, c)
			if i+1 < len(r) {
				i++
				sc.cur = append(sc.cur, r[i])
			}
		case c == '$' && i+1 < len(r) && r[i+1] == '(':
			sc.stack = append(sc.stack, segParen)
			i++
			sc.cut()
		case c == '`':
			sc.backtick()
		case sc.top() == segDouble:
			if c == '"' {
				sc.pop()
			}
			sc.cur = append(sc.cur, c)
		case c == '#' && sc.wordStart(i):
			for i+1 < len(r) && r[i+1] != '\n' {
				i++
			}
		default:
			sc.plain(c, i)
		}
	}
	sc.cut()
	return sc.out
}

// The contexts shellSegments tracks.
const (
	segCommand = iota // top level, or inside $( ... )
	segParen          // inside $( ... )
	segTick           // inside ` ... `
	segDouble         // inside "..."
	segGroup          // inside a subshell's ( ... ), so its ) does not close a $(
)

// segScan is shellSegments' place in a command: the context stack, the segments done, the
// one being read, and whether it is inside single quotes.
type segScan struct {
	r      []rune
	stack  []int
	out    []string
	cur    []rune
	single bool
	// A segment that starts inside double quotes (the text after a substitution closes in
	// "...") is quoted text: an argument of the command before it, never a command.
	quoted bool
}

func (sc *segScan) top() int { return sc.stack[len(sc.stack)-1] }

func (sc *segScan) pop() {
	if len(sc.stack) > 1 {
		sc.stack = sc.stack[:len(sc.stack)-1]
	}
}

// cut ends the segment being read.
func (sc *segScan) cut() {
	if !sc.quoted {
		sc.out = append(sc.out, string(sc.cur))
	}
	sc.cur, sc.quoted = nil, sc.top() == segDouble
}

func (sc *segScan) wordStart(i int) bool {
	return i == 0 || pytext.IsSpace(sc.r[i-1]) || strings.ContainsRune(";&|()`\n", sc.r[i-1])
}

// backtick opens a substitution, or closes the one it is in.
func (sc *segScan) backtick() {
	if sc.top() == segTick {
		sc.pop()
	} else {
		sc.stack = append(sc.stack, segTick)
	}
	sc.cut()
}

// plain reads a rune outside every quote: quotes open, ) closes, operators cut, and a ( at
// a word's start opens a subshell.
func (sc *segScan) plain(c rune, i int) {
	switch {
	case c == '\'':
		sc.single = true
		sc.cur = append(sc.cur, c)
	case c == '"':
		sc.stack = append(sc.stack, segDouble)
		sc.cur = append(sc.cur, c)
	case c == ')':
		if sc.top() == segParen || sc.top() == segGroup {
			sc.pop()
		}
		sc.cut()
	case strings.ContainsRune("\n;&|", c):
		sc.cut()
	case c == '(':
		sc.stack = append(sc.stack, segGroup)
		if sc.wordStart(i) {
			sc.cut()
		} else {
			sc.cur = append(sc.cur, c)
		}
	default:
		sc.cur = append(sc.cur, c)
	}
}

// shellWords is a segment's words with quotes removed; an unclosed quote runs to the end.
func shellWords(s string) []string {
	ws := &wordScan{r: []rune(s)}
	for ws.i = 0; ws.i < len(ws.r); ws.i++ {
		c := ws.r[ws.i]
		switch {
		case ws.single:
			ws.inSingle(c)
		case ws.double:
			ws.inDouble(c)
		case pytext.IsSpace(c):
			if ws.inWord {
				ws.words, ws.w, ws.inWord = append(ws.words, string(ws.w)), nil, false
			}
		default:
			ws.inWord = true
			ws.bare(c)
		}
	}
	if ws.inWord {
		ws.words = append(ws.words, string(ws.w))
	}
	return ws.words
}

// wordScan is shellWords' place in a segment: the word so far, the words done, and the
// quote it is inside.
type wordScan struct {
	r              []rune
	i              int
	w              []rune
	words          []string
	inWord         bool
	single, double bool
}

// inSingle reads a rune inside single quotes, where nothing is special but the closing quote.
func (ws *wordScan) inSingle(c rune) {
	if c == '\'' {
		ws.single = false
	} else {
		ws.w = append(ws.w, c)
	}
}

// inDouble reads a rune inside double quotes, where a backslash escapes only " \ $ and `.
func (ws *wordScan) inDouble(c rune) {
	r := ws.r
	switch {
	case c == '"':
		ws.double = false
	case c == '\\' && ws.i+1 < len(r) && strings.ContainsRune("\"\\$`", r[ws.i+1]):
		ws.i++
		ws.w = append(ws.w, r[ws.i])
	default:
		ws.w = append(ws.w, c)
	}
}

// bare reads a rune of a word outside quotes: a quote opens, a backslash escapes the next rune.
func (ws *wordScan) bare(c rune) {
	switch c {
	case '\'':
		ws.single = true
	case '"':
		ws.double = true
	case '\\':
		if ws.i+1 < len(ws.r) {
			ws.i++
			ws.w = append(ws.w, ws.r[ws.i])
		}
	default:
		ws.w = append(ws.w, c)
	}
}

// stripWrappers drops leading variable assignments and the commands that run another
// command (`timeout 115 gh pr create` is how real PRs were opened), each followed by more.
func stripWrappers(words []string) []string {
	for {
		before := len(words)
		if len(words) > 1 && envAssignment.MatchString(words[0]) {
			words = words[1:]
		}
		if len(words) > 1 {
			switch w := words[0]; {
			case w == "timeout" || w == "env":
				words = afterOptions(words)
			case plainWrappers[w]:
				words = words[1:]
			}
		}
		if len(words) == before {
			return words
		}
	}
}

// afterOptions is what a timeout or env runs: the words after its options (and timeout's
// duration), or all of words when nothing follows them.
func afterOptions(words []string) []string {
	w := words[0]
	valued := map[string]bool{"-k": true, "-s": true, "--kill-after": true, "--signal": true}
	if w == "env" {
		valued = map[string]bool{"-u": true, "--unset": true}
	}
	i := 1
	for i < len(words) && strings.HasPrefix(words[i], "-") && len(words[i]) > 1 {
		if valued[words[i]] && i+1 < len(words) {
			i += 2
		} else {
			i++
		}
	}
	if w == "timeout" {
		i++ // the duration
	}
	if i < len(words) {
		return words[i:]
	}
	return words
}

// isGhPrCreate: gh (any path), its global flags (a flag's value is any word but `pr`),
// then `pr create`.
func isGhPrCreate(words []string) bool {
	if len(words) < 3 || (words[0] != "gh" && !strings.HasSuffix(words[0], "/gh")) {
		return false
	}
	i := 1
	for i < len(words) && strings.HasPrefix(words[i], "-") {
		name, value, hasValue := strings.Cut(words[i], "=")
		if !flagName.MatchString(name) {
			return false
		}
		i++
		if hasValue {
			if value = strings.TrimLeft(value, "="); !flagValue(value) {
				return false
			}
		} else if i < len(words) && flagValue(words[i]) {
			i++
		}
	}
	return i+1 < len(words) && words[i] == "pr" && words[i+1] == "create"
}

// flagValue is a word that can be a flag's value: not empty, not an option, not `pr`.
func flagValue(w string) bool {
	if w == "" || strings.HasPrefix(w, "-") {
		return false
	}
	if !strings.HasPrefix(w, "pr") {
		return true
	}
	rest := []rune(w[2:])
	return len(rest) > 0 && (rest[0] == '_' || pytext.IsWord(rest[0]))
}

// withoutHeredocBodies drops each heredoc's body: it is data, not commands.
func withoutHeredocBodies(command string) string {
	var kept, terminators []string
	for _, line := range strings.Split(command, "\n") {
		if len(terminators) > 0 {
			if pytext.Strip(line) == terminators[0] {
				terminators = terminators[1:]
			}
			continue
		}
		kept = append(kept, line)
		terminators = append(terminators, heredocStarts(line)...)
	}
	return strings.Join(kept, "\n")
}

// heredocStarts is the terminator word of each `<<WORD`, `<<-WORD`, `<<'WORD'` or
// `<<"WORD"` in line, but not a `<<<` here-string, which has no body.
func heredocStarts(line string) []string {
	r := []rune(line)
	var out []string
	for i := 0; i+1 < len(r); i++ {
		if r[i] != '<' || r[i+1] != '<' || (i > 0 && r[i-1] == '<') || (i+2 < len(r) && r[i+2] == '<') {
			continue
		}
		word, last, ok := heredocWord(r, i+2)
		if !ok {
			continue
		}
		out = append(out, word)
		i = last
	}
	return out
}

// heredocWord reads the terminator after a `<<` that ends just before r[j]: an optional `-`,
// blanks, then a name, bare or in matching quotes. last is the index of its final rune.
func heredocWord(r []rune, j int) (word string, last int, ok bool) {
	isName := func(c rune, first bool) bool {
		return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (!first && c >= '0' && c <= '9')
	}
	if j < len(r) && r[j] == '-' {
		j++
	}
	for j < len(r) && pytext.IsSpace(r[j]) {
		j++
	}
	var q rune
	if j < len(r) && (r[j] == '\'' || r[j] == '"') {
		q = r[j]
		j++
	}
	k := j
	for k < len(r) && isName(r[k], k == j) {
		k++
	}
	if k == j || (q != 0 && (k >= len(r) || r[k] != q)) {
		return "", 0, false
	}
	word = string(r[j:k])
	if q != 0 {
		k++
	}
	return word, k - 1, true
}
