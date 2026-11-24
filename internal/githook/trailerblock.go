package githook

import (
	"regexp"
	"strings"
)

// trailerLine is a `Key: value` line as git reads one: a token, optional blanks, then a colon.
var trailerLine = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*[ \t]*:`)

// joinAgentBlock is msg with the paragraphs from its first Agent-* trailer paragraph to its
// end joined into one block, when every paragraph in that run is trailer lines. git reads
// only the last paragraph as trailers, so Agent-* lines an agent wrote a blank line above
// its Co-Authored-By paragraph read as body: their keys were stamped a second time, and the
// stamped ones landed in another block. Comment lines separate paragraphs and are kept, as
// is every message without such a run.
func joinAgentBlock(msg []byte, comment string) []byte {
	lines := strings.SplitAfter(string(msg), "\n")
	paras := paragraphs(lines, comment)
	from := -1
	for p := len(paras) - 1; p >= 1 && isTrailerParagraph(lines[paras[p][0]:paras[p][1]]); p-- {
		if hasAgentKey(lines[paras[p][0]:paras[p][1]]) {
			from = p
		}
	}
	if from < 0 || from == len(paras)-1 {
		return msg
	}
	var b strings.Builder
	for i, l := range lines {
		if i < paras[from][1] || i >= paras[len(paras)-1][0] || strings.TrimSpace(l) != "" {
			b.WriteString(l)
		}
	}
	return []byte(b.String())
}

// paragraphs is each run of lines that are neither blank nor comments, as [first, last+1).
// The first is the subject, which git never reads as trailers.
func paragraphs(lines []string, comment string) [][2]int {
	var out [][2]int
	for i, l := range lines {
		if strings.TrimSpace(l) == "" || isComment(l, comment) {
			continue
		}
		if n := len(out); n > 0 && out[n-1][1] == i {
			out[n-1][1] = i + 1
		} else {
			out = append(out, [2]int{i, i + 1})
		}
	}
	return out
}

// isTrailerParagraph is whether every line is a trailer or continues the one above it.
func isTrailerParagraph(lines []string) bool {
	for i, l := range lines {
		continued := i > 0 && (l[0] == ' ' || l[0] == '\t')
		if !continued && !trailerLine.MatchString(l) {
			return false
		}
	}
	return true
}

// hasAgentKey is whether one of the lines is an Agent-* trailer (keys compare case-blind, as git's do).
func hasAgentKey(lines []string) bool {
	for _, l := range lines {
		if k, _, ok := strings.Cut(l, ":"); ok && strings.HasPrefix(strings.ToLower(k), "agent-") {
			return true
		}
	}
	return false
}
