package why

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"strings"
	"time"
)

// JSONSchema names the shape `livy why --json` writes. Fields are only ever added; a change
// that removes or retypes one is a new schema value.
const JSONSchema = "livy.why/1"

// JSONReport is `livy why --json`: every commit in the range, oldest first, and what was read.
type JSONReport struct {
	Schema  string       `json:"schema"`
	Commits []JSONCommit `json:"commits"`
	Read    JSONRead     `json:"read"`
}

// JSONRead is what a report read: the lines the text report ends with (for people), the
// counts in them, and the folders (or the ledger) that exist but could not be read, which also
// make the command exit 1.
type JSONRead struct {
	Summary string `json:"summary"`
	Events  int    `json:"events"`
	// Files is how many files were read, by source ("ledger", "uploaded segments kept", ...).
	Files      map[string]int `json:"files"`
	Oldest     *time.Time     `json:"oldest_event"`
	Duplicates int            `json:"duplicate_lines"`
	Unreadable int            `json:"unreadable_files_or_lines"`
	Unread     []string       `json:"unread"`
}

// JSONCommit is one commit and the agent work behind it.
type JSONCommit struct {
	SHA       string       `json:"sha"`
	Committed time.Time    `json:"committed"`
	Trailers  JSONTrailers `json:"trailers"`
	// AncestorProblem is why the commit's earlier commits could not be read, so a session's
	// window may start too early; "" when they were.
	AncestorProblem string        `json:"ancestor_problem"`
	Sessions        []JSONSession `json:"sessions"`
	// Squash is the pull request the commit merged and the work behind each of its commits;
	// null when no pull request was looked up.
	Squash *JSONSquash `json:"squash"`
}

// JSONTrailers are the commit's attribution trailers.
type JSONTrailers struct {
	Sessions []string `json:"agent_sessions"`
	Harness  string   `json:"agent_harness"`
	Model    string   `json:"agent_model"`
}

// JSONSquash is the pull request a commit merged.
type JSONSquash struct {
	PR      int          `json:"pr"` // 0 when none was found
	Merge   bool         `json:"merge_commit"`
	Problem string       `json:"problem"`
	Commits []JSONCommit `json:"commits"`
}

// JSONSession is one session's work behind a commit.
type JSONSession struct {
	ID      string `json:"session_id"`
	Harness string `json:"harness"`
	Ticket  string `json:"ticket_key"`
	// From is null when the window runs from the session's start.
	From *time.Time `json:"window_from"`
	To   time.Time  `json:"window_to"`
	// Basis says how From was found, for people; BasisKind and BasisSHA are the same for code.
	Basis     string               `json:"window_basis"`
	BasisKind string               `json:"window_basis_kind"`
	BasisSHA  string               `json:"window_basis_sha"`
	Prompts   []string             `json:"prompt_ids"`
	Tools     map[string]int       `json:"tools"`
	Models    map[string]JSONUsage `json:"models"`
	PerPrompt []JSONPrompt         `json:"per_prompt"`
	// AgentBriefs is the window's agent-written brief, which Prompts leaves out.
	AgentBriefs []string `json:"agent_brief_ids"`
}

// JSONUsage is one model's calls and tokens.
type JSONUsage struct {
	Calls      int64 `json:"calls"`
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
}

// JSONPrompt is what one prompt in a window caused, as --per-prompt prints it.
type JSONPrompt struct {
	ID        string               `json:"prompt_id"`
	At        *time.Time           `json:"sent_at"`
	Mode      string               `json:"permission_mode"`
	Tools     map[string]int       `json:"tools"`
	Results   map[string]int       `json:"results"`
	Approvals map[string]int       `json:"approvals"`
	Models    map[string]JSONUsage `json:"models"`
}

// newJSONReport is the JSON form of commits and what was read.
func newJSONReport(commits []Explained, src *sources) JSONReport {
	out := JSONReport{Schema: JSONSchema, Commits: make([]JSONCommit, 0, len(commits)), Read: readJSON(src)}
	for _, e := range commits {
		out.Commits = append(out.Commits, ToJSON(e))
	}
	return out
}

// readJSON is what src read, as livy.why/1 reports it.
func readJSON(src *sources) JSONRead {
	var b strings.Builder
	writeSources(&b, src)
	return JSONRead{Summary: b.String(), Events: src.events, Files: nonNilMap(maps.Clone(src.files)),
		Oldest: TimeOrNull(src.oldest), Duplicates: src.dupes, Unreadable: src.bad, Unread: nonNil(src.missing)}
}

// ToJSON is one explained commit in its JSON form.
func ToJSON(e Explained) JSONCommit {
	c := e.Commit
	out := JSONCommit{SHA: c.SHA, Committed: c.Committed.UTC(), AncestorProblem: c.AncestorProblem,
		Trailers: JSONTrailers{Sessions: nonNil(c.Sessions), Harness: c.Harness, Model: c.Model},
		Sessions: make([]JSONSession, 0, len(e.Report.Sessions))}
	for _, s := range e.Report.Sessions {
		out.Sessions = append(out.Sessions, sessionJSON(s))
	}
	if sq := e.Squash; sq != nil {
		js := &JSONSquash{PR: sq.PR, Merge: sq.Merge, Problem: sq.Problem, Commits: make([]JSONCommit, 0, len(sq.Originals))}
		if len(sq.Originals) == 0 && js.Problem == "" {
			js.Problem = "no pull request found that merged it"
		}
		for _, o := range sq.Originals {
			js.Commits = append(js.Commits, ToJSON(o))
		}
		out.Squash = js
	}
	return out
}

func sessionJSON(s Session) JSONSession {
	kind, sha := BasisKind(s.Basis)
	out := JSONSession{ID: s.ID, Harness: s.Harness, Ticket: s.Ticket, From: TimeOrNull(s.From), To: s.To.UTC(),
		Basis: s.Basis, BasisKind: kind, BasisSHA: sha, Prompts: nonNil(s.Prompts), Tools: nonNilMap(s.Tools), Models: usageJSON(s.Models),
		PerPrompt: make([]JSONPrompt, 0, len(s.PerPrompt))}
	for _, p := range s.PerPrompt {
		out.PerPrompt = append(out.PerPrompt, JSONPrompt{ID: p.ID, At: TimeOrNull(p.At), Mode: p.Mode,
			Tools: nonNilMap(p.Tools), Results: nonNilMap(p.Results), Approvals: nonNilMap(p.Approvals),
			Models: usageJSON(p.Models)})
	}
	out.AgentBriefs = nonNil(s.AgentBriefs)
	return out
}

// BasisKind is a window basis as a fixed word and the commit it names, if any: "recorded" (the
// session's previous commit.recorded on the branch), "trailer" (its previous commit by
// Agent-Session trailers), "checkout" (the branch's newest checkout, for the session's first
// commit on it), "any_branch" (the session's previous commit.recorded on any branch), "none"
// (no earlier commit: the session's start), else "other".
func BasisKind(basis string) (kind, sha string) {
	switch {
	case strings.HasPrefix(basis, basisAnyBranch):
		return "any_branch", strings.TrimPrefix(basis, basisAnyBranch)
	case strings.HasPrefix(basis, basisRecorded):
		return "recorded", strings.TrimPrefix(basis, basisRecorded)
	case strings.HasPrefix(basis, basisTrailer):
		return "trailer", strings.TrimPrefix(basis, basisTrailer)
	case strings.HasPrefix(basis, basisCheckout):
		return "checkout", ""
	case basis == basisNoEarlier:
		return "none", ""
	}
	return "other", ""
}

func usageJSON(m map[string]Usage) map[string]JSONUsage {
	out := make(map[string]JSONUsage, len(m))
	for k, u := range m {
		out[k] = JSONUsage(u)
	}
	return out
}

// TimeOrNull is t in UTC, nil for the zero time so it encodes as null.
func TimeOrNull(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// nonNil is s, or an empty slice so it encodes as [] rather than null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// nonNilMap is m, or an empty map so it encodes as {} rather than null.
func nonNilMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return map[K]V{}
	}
	return m
}

// WriteJSON writes v as one indented JSON document.
func WriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
}
