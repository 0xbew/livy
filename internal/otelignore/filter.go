package otelignore

import (
	"bufio"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CwdHeader carries the exporting process's working folder, path-escaped, from Claude Code's
// header helper (`livy otel-headers --relay-key`, run in the session's folder) to the
// receiver. The receiver reads it and never stores it.
const CwdHeader = "x-livy-cwd"

// EncodeCwd is a folder as CwdHeader carries it: a header value must be plain ASCII.
func EncodeCwd(dir string) string { return url.PathEscape(dir) }

// sessionKeys are the attributes that name a record's session: Claude Code's session.id,
// Codex's conversation.id, and the thread id Codex puts on some spans (a string; the integer
// thread.id on most spans is an OS thread, not a conversation).
var sessionKeys = []string{claudeSessionKey, "conversation.id", "thread_id", "thread.id"}

const claudeSessionKey = "session.id"

// records is where each signal keeps its resources, and in them its scopes and records.
// Metrics are absent: their data points name no session, so a metrics export without
// CwdHeader can only be judged whole.
var records = map[string][3]string{
	"logs":   {"resourceLogs", "scopeLogs", "logRecords"},
	"traces": {"resourceSpans", "scopeSpans", "spans"},
}

// Filter applies a list to exports.
type Filter struct {
	List   List
	Origin Origin
	// Session is the working folder a session id was opened in, ok false when unknown.
	Session func(id string) (dir string, ok bool)
}

// Dropped is what Apply removed: Ignored, records of a listed project; Unattributed, records
// no project could be told for. An export dropped whole by CwdHeader, or for a shape or signal
// the records cannot be told apart in, counts as one.
type Dropped struct {
	Ignored, Unattributed int
}

// Apply drops from an OTLP/JSON export (decoded, before redaction) every record from an
// ignored project, and with a non-empty list every record whose project is unknown. cwd is
// CwdHeader's value, "" when the export carried none.
func (f Filter) Apply(signal string, doc map[string]any, cwd string) Dropped {
	if f.List.Empty() {
		return Dropped{}
	}
	if cwd != "" {
		dir, err := url.PathUnescape(cwd)
		if err != nil {
			return dropAll(doc, Dropped{Unattributed: 1})
		}
		switch f.List.Folder(dir, f.Origin) {
		case Keep:
			return Dropped{}
		case Ignore:
			return dropAll(doc, Dropped{Ignored: 1})
		case Unknown:
			return dropAll(doc, Dropped{Unattributed: 1})
		}
	}
	keys, ok := records[signal]
	if !ok {
		return dropAll(doc, Dropped{Unattributed: 1})
	}
	j := judge{f: f, verdicts: map[string]Verdict{}}
	if !j.resources(doc, keys) {
		return dropAll(doc, Dropped{Unattributed: 1})
	}
	return j.dropped
}

// dropAll empties doc: an export with no resources stores nothing.
func dropAll(doc map[string]any, d Dropped) Dropped {
	for k := range doc {
		delete(doc, k)
	}
	return d
}

// judge filters one export's records, judging each session once.
type judge struct {
	f        Filter
	verdicts map[string]Verdict
	dropped  Dropped
}

// resources filters every resource's records in place and drops what is left empty; false
// is a shape it cannot walk.
func (j *judge) resources(doc map[string]any, keys [3]string) bool {
	if doc[keys[0]] == nil {
		return true
	}
	rs, ok := doc[keys[0]].([]any)
	if !ok {
		return false
	}
	var kept []any
	for _, r := range rs {
		res, ok := r.(map[string]any)
		if !ok {
			return false
		}
		n, ok := j.scopes(res, keys[1], keys[2])
		if !ok {
			return false
		}
		if n > 0 {
			kept = append(kept, res)
		}
	}
	doc[keys[0]] = kept
	return true
}

// scopes filters one resource's scopes and is how many records it keeps.
func (j *judge) scopes(res map[string]any, scopeKey, recordKey string) (int, bool) {
	ss, ok := res[scopeKey].([]any)
	if !ok {
		return 0, res[scopeKey] == nil
	}
	var kept []any
	total := 0
	for _, s := range ss {
		scope, ok := s.(map[string]any)
		if !ok {
			return 0, false
		}
		recs, ok := scope[recordKey].([]any)
		if !ok && scope[recordKey] != nil {
			return 0, false
		}
		var keep []any
		for _, rec := range recs {
			if j.keeps(rec) {
				keep = append(keep, rec)
			}
		}
		if len(keep) > 0 {
			scope[recordKey] = keep
			kept = append(kept, scope)
			total += len(keep)
		}
	}
	res[scopeKey] = kept
	return total, true
}

// keeps judges one record by its session, counting what it drops.
func (j *judge) keeps(rec any) bool {
	id, codex := sessionOf(rec)
	v, seen := j.verdicts[id]
	if !seen {
		v = Unknown
		// Only a Codex conversation has a rollout to place it by.
		if codex {
			if dir, ok := j.f.Session(id); ok {
				v = j.f.List.Folder(dir, j.f.Origin)
			}
		}
		j.verdicts[id] = v
	}
	switch v {
	case Keep:
		return true
	case Ignore:
		j.dropped.Ignored++
	case Unknown:
		j.dropped.Unattributed++
	}
	return false
}

// attr is one OTLP/JSON attribute, read for its string value only.
type attr struct {
	Key   string `json:"key"`
	Value struct {
		String *string `json:"stringValue"`
	} `json:"value"`
}

// sessionOf is the session a record names ("" for none), and whether it is a Codex
// conversation rather than Claude Code's session.id.
func sessionOf(rec any) (string, bool) {
	m, ok := rec.(map[string]any)
	if !ok {
		return "", false
	}
	raw, err := json.Marshal(m["attributes"])
	if err != nil {
		return "", false
	}
	var attrs []attr
	if json.Unmarshal(raw, &attrs) != nil {
		return "", false
	}
	found := map[string]string{}
	for _, a := range attrs {
		if a.Value.String != nil && *a.Value.String != "" {
			found[a.Key] = *a.Value.String
		}
	}
	for _, k := range sessionKeys {
		if found[k] != "" {
			return found[k], k != claudeSessionKey
		}
	}
	return "", false
}

// RolloutWait is how long a conversation is looked for the first time it is not found:
// Codex sends its first export as it writes the rollout's first line, and that export
// arrives first about as often as not.
const RolloutWait = 2 * time.Second

const rolloutPoll = 100 * time.Millisecond

// CodexSessions finds the folder a Codex conversation was opened in from its rollout
// (<root>/YYYY/MM/DD/rollout-<time>-<id>.jsonl, whose first line is session_meta with the
// cwd). A conversation found is remembered. One not found is waited for until RolloutWait
// after it was first asked for, and looked for again on every later export.
type CodexSessions struct {
	Root  string
	mu    sync.Mutex
	known map[string]string
	until map[string]time.Time // when waiting for a conversation's rollout ends
}

// Folder is the working folder of Codex conversation id.
func (c *CodexSessions) Folder(id string) (string, bool) {
	if id == "" || filepath.Base(id) != id {
		return "", false
	}
	deadline, dir, ok := c.cached(id)
	if ok {
		return dir, true
	}
	for {
		if dir, ok := c.find(id); ok {
			return dir, true
		}
		if !time.Now().Before(deadline) {
			return "", false
		}
		time.Sleep(rolloutPoll)
	}
}

// cached is id's remembered folder, else until when to wait for its rollout: RolloutWait
// from the first time id was asked for, so exports of a new conversation that arrive
// together all wait for it.
func (c *CodexSessions) cached(id string) (time.Time, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dir, ok := c.known[id]; ok {
		return time.Time{}, dir, true
	}
	if until, ok := c.until[id]; ok {
		return until, "", false
	}
	if c.until == nil {
		c.until = map[string]time.Time{}
	}
	c.until[id] = time.Now().Add(RolloutWait)
	return c.until[id], "", false
}

// find reads id's rollout, and remembers its folder.
func (c *CodexSessions) find(id string) (string, bool) {
	paths, _ := filepath.Glob(filepath.Join(c.Root, "*", "*", "*", "rollout-*-"+id+".jsonl"))
	for _, p := range paths {
		if dir, ok := rolloutCwd(p, id); ok {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.known == nil {
				c.known = map[string]string{}
			}
			c.known[id] = dir
			return dir, true
		}
	}
	return "", false
}

// rolloutCwd is the cwd a rollout's session_meta line records for conversation id.
func rolloutCwd(path, id string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }() // read only: a failed close loses nothing
	var first struct {
		Type    string `json:"type"`
		Payload struct {
			ID  string `json:"id"`
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.NewDecoder(bufio.NewReader(f)).Decode(&first) != nil || first.Type != "session_meta" ||
		first.Payload.ID != id || !filepath.IsAbs(first.Payload.Cwd) {
		return "", false
	}
	return first.Payload.Cwd, true
}
