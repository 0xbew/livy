package claudecode

import (
	"strings"

	"github.com/0xbew/livy/internal/extract"
	"github.com/0xbew/livy/internal/pytext"
)

// Prompt is one human prompt with the words around it, raw and uncut: the input of prompt
// capture. Raw text lives only in what PromptsIn returns.
type Prompt struct {
	ID, Repo, Branch, SessionID, Text, AgentBefore string
	// HarnessID is the harness's own id for the prompt (Claude Code's promptId), "" when none.
	HarnessID string
	TS        any
	Exchanges []Exchange
}

// PromptRead is what one read of a transcript found.
type PromptRead struct {
	Found     []Prompt
	Offset    int64 // where the next read starts
	Bad       int   // unreadable lines
	NoSession []string
	// Held says why the read stopped before the end, at Offset: a record it does not recognise,
	// which may be a prompt in a new format. "" when it read to the end.
	Held string
	// Outside is every prompt with a session sent from a folder outside every in-scope repo,
	// from the whole file, with no repo, branch or words around it. Only the vault reads them,
	// and only for a session that committed to an in-scope repo.
	Outside []Prompt
}

// Exchange is one earlier turn: what the person said, then what the agent said.
type Exchange struct{ Person, Agent string }

const (
	exchangesKept = 3
	sdkTurn       = "sdk"
)

// TranscriptFiles lists every transcript under root, with its project-relative name.
func TranscriptFiles(root string) (relative, path []string) {
	var unreadable int
	for _, f := range transcriptFiles(root, &unreadable) {
		relative, path = append(relative, f.relative), append(path, f.path)
	}
	return relative, path
}

// PathKey is the state key of a transcript's project-relative name.
func PathKey(relative string) string { return pathKey(relative) }

// FirstLineHash is the hash of a transcript's first complete line.
func FirstLineHash(path string) (string, error) { return firstLineHash(path) }

func assistantText(rec map[string]any) string {
	msg, _ := rec["message"].(map[string]any)
	content, ok := msg["content"].([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, b := range content {
		if m, ok := b.(map[string]any); ok && m["type"] == "text" {
			if t, ok := m["text"].(string); ok {
				parts = append(parts, t)
			}
		}
	}
	return pytext.Strip(strings.Join(parts, " "))
}

// exchangeText is a human turn as the labeller's tested context counted one.
func exchangeText(rec map[string]any) string {
	if pytext.Truthy(rec["isMeta"]) || pytext.Truthy(rec["isVisibleInTranscriptOnly"]) || pytext.Truthy(rec["isSidechain"]) {
		return ""
	}
	msg, _ := rec["message"].(map[string]any)
	text := pytext.Strip(humanText(msg["content"]))
	if text == "" || strings.HasPrefix(text, "<command-") {
		return ""
	}
	for _, p := range harnessMessagePrefixes {
		if strings.HasPrefix(text, p) {
			return ""
		}
	}
	return text
}

// PromptsIn is the prompts at or after byte start, the offset read to, the unreadable lines,
// and the ids of prompts with no session. The whole file is read: the exchanges before a new
// prompt may lie before start. A last line with no newline yet is left for the next pass. A
// user message at or after holdFrom of a shape this reader does not know stops the read before
// it (Held); one before holdFrom was read past by an earlier pass and is passed again.
func PromptsIn(scope *extract.Scope, path string, start, holdFrom int64) (PromptRead, error) {
	s := &promptScan{rd: &reader{scope: scope}, start: start, holdFrom: holdFrom}
	offset, bad, err := extract.ReadRecords(path, s.record)
	if err != nil {
		return PromptRead{}, err
	}
	r := PromptRead{Found: s.found, Offset: offset, Bad: bad, NoSession: s.noSession, Outside: s.outside}
	if s.held != "" {
		r.Offset, r.Held = s.heldAt, s.held
	}
	return r, nil
}

// Exchanges is the earlier turns a prompt carries: the finished exchanges, newest last and
// at most exchangesKept, and the one still open.
type Exchanges struct {
	done      []Exchange
	person    string // the open exchange's human turn
	open      bool
	agentSaid []string // what the agent has said since the open human turn
}

// AgentSaid adds the agent's words to the open exchange.
func (x *Exchanges) AgentSaid(text string) { x.agentSaid = append(x.agentSaid, text) }

// Last is the last exchangesKept exchanges, the open one included, in a slice of its own.
func (x *Exchanges) Last() []Exchange {
	ex := append([]Exchange(nil), x.done...)
	if x.open {
		ex = append(ex, Exchange{x.person, strings.Join(x.agentSaid, " ")})
	}
	if len(ex) > exchangesKept {
		ex = ex[len(ex)-exchangesKept:]
	}
	return ex
}

// PersonSaid closes the open exchange and opens one at a human turn. A turn outside any repo
// opens none and forgets the finished ones.
func (x *Exchanges) PersonSaid(text string, inRepo bool) {
	if x.open {
		x.done = append(x.done, Exchange{x.person, strings.Join(x.agentSaid, " ")})
		if len(x.done) > exchangesKept {
			x.done = x.done[len(x.done)-exchangesKept:]
		}
	}
	x.person, x.open, x.agentSaid = text, inRepo, nil
	if !inRepo {
		x.done = nil
	}
}

// promptScan is PromptsIn's state from one record to the next.
type promptScan struct {
	rd         *reader
	start      int64
	holdFrom   int64
	ctx        ctxT
	afterError bool
	lastAgent  string // the agent's last words before the next prompt
	exchanges  Exchanges
	found      []Prompt
	outside    []Prompt
	noSession  []string
	held       string // why the scan stopped, "" while it reads on
	heldAt     int64
}

// record reads one transcript record that starts at byte here.
func (s *promptScan) record(rec map[string]any, here int64) {
	if s.held != "" {
		return
	}
	if here >= s.holdFrom && rec["type"] == "user" && !userShapeKnown(rec) {
		s.held, s.heldAt = unknownUserShape+" (Claude Code "+recordVersion(rec)+")", here
		return
	}
	if own := s.rd.contextFrom(rec); own != nil {
		s.ctx = *own
	}
	if rec["type"] == "assistant" && rec["isSidechain"] != true && s.ctx.Repo != "" {
		if t := assistantText(rec); t != "" {
			s.lastAgent = t
			s.exchanges.AgentSaid(t)
		}
	}
	items, ae, _ := s.rd.interactionsOf(rec, s.ctx, s.afterError, here)
	s.afterError = ae
	for _, it := range items {
		s.prompt(rec, it, here)
	}
	if rec["type"] != "user" {
		return
	}
	if human := exchangeText(rec); human != "" {
		s.exchanges.PersonSaid(human, s.ctx.Repo != "")
		if s.ctx.Repo == "" {
			s.lastAgent = ""
		}
	}
}

// prompt keeps one interaction when it is a human prompt: in Found when it is in a repo at or
// after start, in Outside when it is outside every repo.
func (s *promptScan) prompt(rec map[string]any, it interaction, here int64) {
	if it.Kind != "prompt" || (it.SlashCommand != nil && *it.SlashCommand) || rec["turnOrigin"] == sdkTurn {
		return
	}
	outside := it.Repo == ""
	if !outside && here < s.start {
		return
	}
	sid, _ := it.SessionID.(string)
	if sid == "" {
		if !outside {
			s.noSession = append(s.noSession, it.ID)
		}
		return
	}
	msg, _ := rec["message"].(map[string]any)
	harnessID, _ := rec["promptId"].(string)
	p := Prompt{ID: it.ID, HarnessID: harnessID, TS: it.Timestamp, SessionID: sid, Text: pytext.Strip(humanText(msg["content"]))}
	if outside {
		s.outside = append(s.outside, p)
		return
	}
	p.Repo, p.Branch, p.AgentBefore = it.Repo, it.Branch, s.lastAgent
	p.Exchanges = append(p.Exchanges, s.exchanges.Last()...)
	s.found = append(s.found, p)
}
