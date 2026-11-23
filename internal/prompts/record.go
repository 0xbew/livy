package prompts

import (
	"strings"
	"unicode/utf8"

	"github.com/0xbew/scrub"

	"github.com/0xbew/livy/internal/extract"
	"github.com/0xbew/livy/internal/extract/claudecode"
	"github.com/0xbew/livy/internal/pytext"
)

// What a text record holds: the labeller's tested shape.
const (
	contextChars     = 1500
	agentBeforeChars = 4000
	// Withheld replaces a context field that came from a session outside the configured scope.
	Withheld = "[relay from outside the configured orgs withheld]"
)

type cutter func(text string, d *scrub.Denylist, limit int) string

// whole is the text through the scrubber with a limit no placeholder can reach, so the
// scrubber itself never cuts.
func whole(text string, d *scrub.Denylist) string {
	return scrub.Scrub(text, d, 2*utf8.RuneCountInString(text)+100)
}

func runes(s string) []rune { return []rune(s) }

// head scrubs the first limit+margin characters, drops the scrubbed margin, then keeps at
// most limit, never ending on part of a token. The margin is dropped from the SCRUBBED text:
// a redaction that shortens the window would otherwise pull a name split by the window's edge
// back inside what is kept.
func head(text string, d *scrub.Denylist, limit int) string {
	r := runes(text)
	if len(r) <= limit+scrub.RedactMargin {
		return scrub.Scrub(text, d, limit)
	}
	clean := runes(whole(string(r[:limit+scrub.RedactMargin]), d))
	kept := string(clean[:max(0, len(clean)-scrub.RedactMargin)])
	if kr := runes(kept); len(kr) > limit {
		kept = string(kr[:limit])
	}
	if !strings.Contains(pytext.Strip(kept), " ") {
		return scrub.Cut
	}
	s := strings.TrimRightFunc(kept, pytext.IsSpace)
	i := strings.LastIndexFunc(s, pytext.IsSpace)
	return strings.TrimRightFunc(s[:i], pytext.IsSpace) + " " + scrub.Cut
}

// tail is the same from the other end: scrub the last limit+margin characters, drop the
// scrubbed margin, then keep the last limit, never beginning on part of a token.
func tail(text string, d *scrub.Denylist, limit int) string {
	r := runes(text)
	if len(r) <= limit {
		return whole(text, d)
	}
	clean := runes(whole(string(r[max(0, len(r)-(limit+scrub.RedactMargin)):]), d))
	if len(r) > limit+scrub.RedactMargin {
		clean = clean[min(scrub.RedactMargin, len(clean)):]
	}
	if len(clean) > limit {
		clean = clean[len(clean)-limit:]
	}
	kept := string(clean)
	if !strings.Contains(pytext.Strip(kept), " ") {
		return scrub.Cut
	}
	s := strings.TrimLeftFunc(kept, pytext.IsSpace)
	i := strings.IndexFunc(s, pytext.IsSpace)
	return scrub.Cut + " " + strings.TrimLeftFunc(s[i:], pytext.IsSpace)
}

// record is one text record: each field scrubbed over what it keeps, then cut.
type record struct {
	ID, At       any
	Context      string
	Conversation []claudecode.Exchange
	AgentBefore  string
	Prompt       string
	DenyVersion  int
}

func textRecord(p claudecode.Prompt, d *scrub.Denylist) *record {
	kept := func(text string, cut cutter, limit int) string {
		text = strings.ToValidUTF8(text, "�")
		if cut == nil {
			return whole(text, d)
		}
		return cut(text, d, limit)
	}
	ts := extract.PyStr(p.TS)
	minute := string([]rune(ts)[:min(16, utf8.RuneCountInString(ts))])
	repo := p.Repo[strings.Index(p.Repo, "/")+1:]
	branch := p.Branch
	if branch == "" {
		branch = "?"
	}
	rec := &record{ID: p.ID, At: p.TS, DenyVersion: d.Version,
		Context:     kept(minute+" · "+repo+" · branch `"+branch+"`", nil, 0),
		AgentBefore: kept(p.AgentBefore, tail, agentBeforeChars),
		Prompt:      kept(p.Text, nil, 0)}
	for _, e := range p.Exchanges {
		rec.Conversation = append(rec.Conversation, claudecode.Exchange{
			Person: kept(e.Person, head, contextChars), Agent: kept(e.Agent, tail, contextChars)})
	}
	return rec
}

// fields is the record as the audit reads it: the text fields only.
func (r *record) fields() map[string]any {
	conv := []any{}
	for _, e := range r.Conversation {
		conv = append(conv, map[string]any{"person": e.Person, "agent": e.Agent})
	}
	return map[string]any{"context": r.Context, "conversation": conv, "agent_before": r.AgentBefore, "prompt": r.Prompt}
}

// json is the queued line.
func (r *record) json() string {
	m := r.fields()
	m["schema"], m["prompt_id"], m["captured_at"], m["ts"] = 1, r.ID, r.At, r.At
	m["scrubber_version"], m["denylist_version"] = scrub.Version, r.DenyVersion
	return dumps(m, false)
}

// contextTexts is a record's context fields exactly as they would leave: what the relay rule
// judges them on.
func (r *record) contextTexts() []string {
	out := []string{r.AgentBefore}
	for _, e := range r.Conversation {
		out = append(out, e.Person, e.Agent)
	}
	return out
}

// withholdOutside replaces every context field whose kept text the relay rule attributes to an
// outside source. raw is the prompt the record was built from, so a field can be judged on its
// text as the source session said it as well as on what would leave.
func (r *record) withholdOutside(raw claudecode.Prompt, outside func(string) bool) int {
	n := 0
	if outside(raw.AgentBefore) || outside(r.AgentBefore) {
		r.AgentBefore, n = Withheld, n+1
	}
	for i := range r.Conversation {
		rawEx := claudecode.Exchange{}
		if i < len(raw.Exchanges) {
			rawEx = raw.Exchanges[i]
		}
		if outside(rawEx.Person) || outside(r.Conversation[i].Person) {
			r.Conversation[i].Person, n = Withheld, n+1
		}
		if outside(rawEx.Agent) || outside(r.Conversation[i].Agent) {
			r.Conversation[i].Agent, n = Withheld, n+1
		}
	}
	return n
}

// Scrubbed is text through the scrubber whole, never cut: what `livy show` prints and what an
// upload would carry.
func Scrubbed(text string, d *scrub.Denylist) string {
	return whole(strings.ToValidUTF8(text, "�"), d)
}
