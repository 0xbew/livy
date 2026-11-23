// Package ticket derives the ticket key that groups a unit of work: every session, prompt
// and commit made for one ticket. The key is a join key, so work with none gets the explicit
// None, never an empty or absent value.
package ticket

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/0xbew/livy/internal/config"
)

// DefaultRegex is a Jira-style key: an upper-case project key, a dash, a number, not inside
// a longer word. It is case-sensitive on purpose: lower-case it matches package versions and
// numbered branch names (duckdb-2, gates-2). It still matches tokens like UTF-8.
const DefaultRegex = `(?:^|[^A-Za-z0-9])([A-Z][A-Z0-9]+-\d+)`

// None is the key of work no ticket key was found for.
const None = "no-ticket"

// SettingKey is the setting that replaces DefaultRegex (LIVY_TICKET_REGEX in the environment).
const SettingKey = "ticket.regex"

// Matcher finds a ticket key with one regular expression.
type Matcher struct{ re *regexp.Regexp }

// New compiles pattern (Go's RE2 syntax).
func New(pattern string) (*Matcher, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("ticket pattern: %w", err)
	}
	return &Matcher{re: re}, nil
}

// Key is the first ticket key in sources, tried in order (the branch, then the commit
// message), or None. A pattern with a capture group yields the group. Keys are upper-cased so
// that a pattern written case-insensitively groups one ticket under one spelling.
func (m *Matcher) Key(sources ...string) string {
	for _, s := range sources {
		sub := m.re.FindStringSubmatch(s)
		if sub == nil {
			continue
		}
		k := sub[0]
		if len(sub) > 1 {
			k = sub[1]
		}
		if k = strings.ToUpper(strings.TrimSpace(k)); k != "" {
			return k
		}
	}
	return None
}

var defaultMatcher = regexp.MustCompile(DefaultRegex)

// Default is the matcher for DefaultRegex.
func Default() *Matcher { return &Matcher{re: defaultMatcher} }

// FromSettings is the matcher the settings name: ticket.regex, else DefaultRegex. A value
// that is not a string or does not compile falls back to DefaultRegex, and problem says so.
func FromSettings(s config.Resolved) (m *Matcher, problem string) {
	v, ok := s.Settings[SettingKey]
	if !ok {
		return Default(), ""
	}
	pattern, isString := v.Value.(string)
	if !isString {
		return Default(), fmt.Sprintf("%s from %s is not a string; the default %s is used", SettingKey, v.Layer, DefaultRegex)
	}
	m, err := New(pattern)
	if err != nil {
		return Default(), fmt.Sprintf("%s from %s does not compile (%v); the default %s is used", SettingKey, v.Layer, err, DefaultRegex)
	}
	return m, ""
}
