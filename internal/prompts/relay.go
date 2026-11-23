package prompts

import (
	"bytes"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/extract"
	"github.com/0xbew/livy/internal/gitinfo"
	"github.com/0xbew/livy/internal/pytext"
)

// The relay rule: a prompt is a relay when the agent output of ONE earlier session, other
// than its own, holds at least half of the prompt's 8-word shingles within 14 days. The
// numbers are the labeller's tested ones and are not settings.
const (
	shingleWords = 8
	most         = 0.5
	window       = 14 * 24 * time.Hour
	common       = 40
	// Outside is a source that cannot be shown to be in scope.
	Outside = "outside"
)

var marks = regexp.MustCompile("[*`#>|_~•▪●◦\\-]+")

func words(text string) []string {
	return strings.FieldsFunc(marks.ReplaceAllString(strings.ToLower(text), " "), pytext.IsSpace)
}

func shingles(text string) map[string]bool {
	t := words(text)
	out := map[string]bool{}
	for i := 0; i+shingleWords <= len(t); i++ {
		out[strings.Join(t[i:i+shingleWords], " ")] = true
	}
	return out
}

var naiveLayouts = []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04", "2006-01-02"}

// aware is an instant, ok false for text that is not a timestamp, and naive true for one with
// no zone: it cannot be compared with an aware prompt time, and guessing its zone could prove
// a relay that never happened.
func aware(ts any) (t time.Time, ok, naive bool) {
	if p := extract.UTCInstant(ts); p != nil {
		return *p, true, false
	}
	s, isStr := ts.(string)
	if !isStr {
		return t, false, false
	}
	s = pytext.Strip(s)
	for _, l := range naiveLayouts {
		if _, err := time.Parse(l, s); err == nil {
			return t, false, true
		}
	}
	return t, false, false
}

type appearance struct {
	at  time.Time
	cwd string
	has bool
}

// Sources is what the transcripts say about the wanted shingles: for each, the sessions it
// appears in (all of them, for the boilerplate count) and every time it appears in each,
// with the working directory of the record that held it.
type Sources struct {
	wanted     map[string]bool
	firstWords map[string]bool
	seen       map[string]map[string][]appearance
	Problems   map[string]int
}

func newSources(wanted map[string]bool) *Sources {
	s := &Sources{wanted: wanted, firstWords: map[string]bool{}, seen: map[string]map[string][]appearance{}, Problems: map[string]int{}}
	for w := range wanted {
		s.firstWords[strings.SplitN(w, " ", 2)[0]] = true
	}
	return s
}

// assistantText is the text blocks of one assistant record, ok false (reason counted) for a
// shape not known here.
func (s *Sources) assistantText(rec map[string]any) (string, bool) {
	msg, ok := rec["message"].(map[string]any)
	if !ok {
		s.Problems["message_shape"]++
		return "", false
	}
	switch c := msg["content"].(type) {
	case string:
		return c, true
	case []any:
		var parts []string
		for _, b := range c {
			m, ok := b.(map[string]any)
			if !ok {
				s.Problems["block_shape"]++
				return "", false
			}
			if m["type"] != "text" {
				continue
			}
			t, ok := m["text"].(string)
			if !ok {
				s.Problems["text_shape"]++
				return "", false
			}
			parts = append(parts, t)
		}
		return strings.Join(parts, " "), true
	}
	s.Problems["content_shape"]++
	return "", false
}

// addText records the wanted shingles of one agent message.
func (s *Sources) addText(session string, when time.Time, cwd string, text string) {
	t := words(text)
	for i := 0; i+shingleWords <= len(t); i++ {
		if !s.firstWords[t[i]] {
			continue
		}
		sh := strings.Join(t[i:i+shingleWords], " ")
		if s.wanted[sh] {
			bySession := s.seen[sh]
			if bySession == nil {
				bySession = map[string][]appearance{}
				s.seen[sh] = bySession
			}
			bySession[session] = append(bySession[session], appearance{when, cwd, cwd != ""})
		}
	}
}

func (s *Sources) addLine(line []byte) {
	v, err := extract.Decode(line)
	if err != nil {
		s.Problems["bad_json"]++
		return
	}
	rec, ok := v.(map[string]any)
	if !ok {
		s.Problems["record_shape"]++
		return
	}
	if rec["type"] != "assistant" {
		return
	}
	session, _ := rec["sessionId"].(string)
	if session == "" {
		// Never pooled under a shared key: it could pose as another session and prove a false relay.
		s.Problems["no_session"]++
		return
	}
	when, ok, naive := aware(rec["timestamp"])
	if naive {
		s.Problems["source_time_naive"]++
		return
	}
	if !ok {
		s.Problems["source_time_unreadable"]++
		return
	}
	text, ok := s.assistantText(rec)
	if !ok {
		return
	}
	cwd, _ := rec["cwd"].(string)
	s.addText(session, when, cwd, text)
}

func (s *Sources) addFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		s.Problems["file_unreadable"]++
		return
	}
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break // still being written: read whole on a later pass
		}
		s.addLine([]byte(pytext.DecodeReplace(data[:i+1])))
		data = data[i+1:]
	}
}

// Proof is one prompt's relay facts.
type Proof struct {
	Proof   string
	Share   float64
	Source  *string
	LagMins *float64
}

func (s *Sources) match(prompt, own string, at time.Time, repoOf func(string, bool) string) Proof {
	mine := shingles(prompt)
	res := Proof{Proof: "no"}
	if len(mine) == 0 {
		return res
	}
	t := s.tally(mine, own, at)
	if len(t.hits) == 0 {
		return res
	}
	best := t.best()
	res.Share = float64(t.hits[best]) / float64(len(mine))
	if res.Share >= most {
		src := sourceOf(t.where[best], repoOf)
		lag := at.Sub(t.latest[best]).Seconds() / 60
		res.Proof, res.Source, res.LagMins = "yes", &src, &lag
	}
	return res
}

// relayTally is, per other session, how many of a prompt's shingles it said in the window
// before the prompt, when it last said one, and where it was when it did.
type relayTally struct {
	hits   map[string]int
	latest map[string]time.Time
	where  map[string]map[appearance2]bool
}

// tally counts each shingle of mine that another session said shortly before at. A shingle
// too common to prove anything is skipped.
func (s *Sources) tally(mine map[string]bool, own string, at time.Time) relayTally {
	t := relayTally{hits: map[string]int{}, latest: map[string]time.Time{}, where: map[string]map[appearance2]bool{}}
	for sh := range mine {
		sessions := s.seen[sh]
		if len(sessions) > common {
			continue
		}
		for session, times := range sessions {
			if session == own {
				continue
			}
			best := latestBefore(times, at)
			if best == nil {
				continue
			}
			t.hits[session]++
			if t.where[session] == nil {
				t.where[session] = map[appearance2]bool{}
			}
			t.where[session][appearance2{best.cwd, best.has}] = true
			if l, ok := t.latest[session]; !ok || best.at.After(l) {
				t.latest[session] = best.at
			}
		}
	}
	return t
}

// latestBefore is the last of times inside the window before at, or nil.
func latestBefore(times []appearance, at time.Time) *appearance {
	var best *appearance
	for i := range times {
		t := times[i]
		if t.at.Before(at) && at.Sub(t.at) <= window && (best == nil || t.at.After(best.at)) {
			best = &times[i]
		}
	}
	return best
}

// best is the session with the most hits, the later one on a tie, then the first by name.
func (t relayTally) best() string {
	var names []string
	for k := range t.hits {
		names = append(names, k)
	}
	sort.Strings(names)
	best := names[0]
	for _, n := range names[1:] {
		if t.hits[n] > t.hits[best] || (t.hits[n] == t.hits[best] && t.latest[n].After(t.latest[best])) {
			best = n
		}
	}
	return best
}

// sourceOf is the one repo every appearance worked in, else Outside.
func sourceOf(where map[appearance2]bool, repoOf func(string, bool) string) string {
	repos := map[string]bool{}
	for a := range where {
		repos[repoOf(a.cwd, a.has)] = true
	}
	src := Outside
	if len(repos) == 1 {
		for r := range repos {
			src = r
		}
	}
	return src
}

type appearance2 struct {
	cwd string
	has bool
}

// sourceRepo is owner/name from the git remote of a source message's working directory when
// that remote is in scope, else Outside. There is no walking up to a parent: a directory that
// is gone cannot be verified. A lookup that timed out is Outside too, and counted.
func sourceRepo(scope *extract.Scope, problems map[string]int) func(string, bool) string {
	cache := map[string]string{}
	return func(cwd string, has bool) string {
		if !has || !strings.HasPrefix(cwd, "/") {
			return Outside
		}
		if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
			return Outside
		}
		if r, ok := cache[cwd]; ok {
			return r
		}
		scope.Git.ResetTimedOut()
		url := scope.Git.Origin(cwd)
		ours := url != "" && scope.Git.InScope(url, scope.Orgs)
		if !ours && scope.Git.TimedOut() {
			problems["source_scope_timeout"]++
		}
		r := Outside
		if ours {
			if _, owner, name, ok := gitinfo.RemoteOwnerRepo(url); ok && name != "" {
				r = owner + "/" + name
			}
		}
		cache[cwd] = r
		return r
	}
}
