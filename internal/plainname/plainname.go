// Package plainname is the rule for a NAME on a ledger line (a tool, a skill, an error kind,
// a stop reason): short, and unable to carry a path or a sentence.
package plainname

import (
	"unicode/utf8"

	"github.com/0xbew/livy/internal/pytext"
)

// MaxChars bounds a name. A value past it is not a name.
const MaxChars = 200

// Valid is true for a plain name: 1..200 characters, none of them whitespace or a slash.
func Valid(s string) bool {
	n := utf8.RuneCountInString(s)
	if n < 1 || n > MaxChars {
		return false
	}
	for _, r := range s {
		if pytext.IsSpace(r) || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}
