package githook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/extract/claudecode"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/ledger"
)

// A session's model is stamped only when every agent in it used the same one since the
// session's previous commit on this branch: the window `livy why` reports the commit's work in.
// A subagent's shell carries its parent's session id, so a Sonnet subagent committing inside
// an Opus session looks, from the environment, exactly like the Opus session committing; and a
// subagent that finished before the commit still did part of its work.

// maxTranscriptLine is the longest transcript line read when a whole transcript is read.
const maxTranscriptLine = 64 << 20

// transcriptTail is how much of a transcript's end is read for its newest assistant message.
const transcriptTail = 8 << 20

// syntheticModel is Claude Code's model name on a message it wrote itself (an error notice).
const syntheticModel = "<synthetic>"

// recordedFacts is what Livy can read about the session: the ledger's lines for it (until
// they ship), then, for Claude Code, the session's transcripts (the main one and each
// subagent's), which outlive the ledger.
func recordedFacts(a agent, source string) sessionFacts {
	from := "HEAD"
	if source == "commit" {
		from = "HEAD^" // an amend replaces HEAD; -c and -C only widen the window, which can only unstamp
	}
	since := previousCommit(a.session, from)
	models := map[string]bool{}
	version := ledgerFacts(a.session, since, models)
	switch a.harness {
	case harnessClaudeCode:
		if v := transcriptFacts(a.session, since, models); v != "" {
			version = v
		}
	case harnessCodex:
		if v := codexRolloutFacts(a.session, since, models); v != "" {
			version = v
		} else if v := os.Getenv("CODEX_VERSION"); v != "" {
			version = v
		}
	}
	f := sessionFacts{version: version}
	if len(models) == 1 {
		for m := range models {
			f.model = m // "" alone (no model readable) stamps nothing
		}
	}
	return f
}

// previousCommit is when the newest commit at or behind from carrying this session's
// Agent-Session trailer was committed (the date `livy why` reads); the zero time (the whole
// session) when there is none or git cannot say, which can only leave the model unstamped.
func previousCommit(session, from string) time.Time {
	// --grep only narrows the walk: it also matches a message that quotes the trailer in its
	// body, so each candidate's parsed trailer must name the session exactly.
	out, err := gitRun(hookTimeout(), nil, "log", "--format=%cI%x00%(trailers:key=Agent-Session,valueonly,separator=%x00)%x1e",
		"--fixed-strings", "--grep=Agent-Session: "+session, from, "--")
	if err != nil {
		return time.Time{}
	}
	for rec := range strings.SplitSeq(string(out), "\x1e") {
		date, sessions, _ := strings.Cut(strings.TrimSpace(rec), "\x00")
		if !slices.Contains(strings.Split(sessions, "\x00"), session) {
			continue
		}
		t, err := time.Parse(time.RFC3339, date)
		if err != nil {
			return time.Time{}
		}
		return t
	}
	return time.Time{}
}

// ledgerLine is the part of a ledger line that says which model a session used.
type ledgerLine struct {
	Event          string `json:"event"`
	TS             string `json:"ts"`
	SessionID      string `json:"session_id"`
	Model          string `json:"model"`
	ToModel        string `json:"to_model"`
	CCVersion      string `json:"cc_version"`
	HarnessVersion string `json:"harness_version"`
}

// ledgerFacts adds the models the session's ledger lines since `since` name, and returns the
// newest harness version any of its lines names.
func ledgerFacts(session string, since time.Time, models map[string]bool) (version string) {
	f, err := os.Open(filepath.Join(hook.DataDir(), ledger.FileName))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }() // opened read-only: a close error loses nothing
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	for sc.Scan() {
		var l ledgerLine
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.SessionID != session {
			continue
		}
		if v := l.CCVersion + l.HarnessVersion; v != "" {
			version = v
		}
		ts, err := time.Parse(time.RFC3339, l.TS)
		if err != nil || ts.Before(since) {
			continue
		}
		if m := l.ToModel; l.Event == "model.switched" && m != "" {
			models[m] = true
		} else if l.Model != "" {
			models[l.Model] = true
		}
	}
	return version
}

// transcriptFacts adds the model of every assistant message since `since` in each of the
// session's transcripts (the main one and each subagent's), and returns the Claude Code version
// of the main one's newest assistant message. A transcript written since `since` whose model
// cannot be read adds "", which never agrees with a real model: missing evidence is not agreement.
func transcriptFacts(session string, since time.Time, models map[string]bool) (version string) {
	mains, _ := filepath.Glob(filepath.Join(claudecode.LiveRoot(), "*", session+".jsonl"))
	for _, main := range mains {
		_, version = newestAssistant(main)
		subs, _ := filepath.Glob(filepath.Join(filepath.Dir(main), session, "subagents", "*.jsonl"))
		for _, path := range append([]string{main}, subs...) {
			if len(models) > 1 {
				return version // two models already: nothing more can stamp one
			}
			if active(path, since) {
				modelsSince(path, since, models)
			}
		}
	}
	return version
}

// active is whether the transcript was written since `since`.
func active(path string, since time.Time) bool {
	st, err := os.Stat(path)
	return err == nil && !st.ModTime().Before(since)
}

// transcriptLine is the part of a transcript line that names a message's model.
type transcriptLine struct {
	Type      string `json:"type"`
	Version   string `json:"version"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Model string `json:"model"`
	} `json:"message"`
}

// newestAssistant is the model and Claude Code version of the newest assistant message in the
// transcript's last transcriptTail bytes; "" when there is none.
func newestAssistant(path string) (model, version string) {
	buf, start := tail(path)
	lines := bytes.Split(buf, []byte("\n"))
	for i, line := range slices.Backward(lines) {
		if start > 0 && i == 0 {
			break // the first line read may start mid-line
		}
		if !bytes.Contains(line, []byte(`"assistant"`)) {
			continue
		}
		var l transcriptLine
		if json.Unmarshal(line, &l) == nil && l.Type == "assistant" && l.Message.Model != "" && l.Message.Model != syntheticModel {
			return l.Message.Model, l.Version
		}
	}
	return "", ""
}

// modelsSince adds the model of each assistant message in the transcript written at or after
// since, or "" when those since then name none. Transcripts are appended in time order, so it
// reads back from the end, doubling how much it reads, until it meets an older line or the
// file's start.
func modelsSince(path string, since time.Time, models map[string]bool) {
	read := func() (window, bool) { return backFrom(path, since) }
	if since.IsZero() {
		read = func() (window, bool) { return wholeFile(path) } // no earlier commit: forward once, not in doubling tails
	}
	w, ok := read()
	if !ok {
		models[""] = true // written lately but unreadable: missing evidence is not agreement
		return
	}
	if w.lines > 0 && len(w.models) == 0 {
		models[""] = true
	}
	maps.Copy(models, w.models)
}

// backFrom reads the transcript back from its end, doubling how much it reads, until it meets
// an assistant line older than since or the file's start; ok false when it cannot be read.
func backFrom(path string, since time.Time) (w window, ok bool) {
	for size := int64(transcriptTail); ; size *= 2 {
		buf, start := tailOf(path, size)
		if buf == nil {
			return window{}, false
		}
		w = scanModels(buf, start > 0, since)
		if w.older || start == 0 || len(w.models) > 1 {
			return w, true
		}
	}
}

// wholeFile reads every line of the transcript, a line at a time; ok false when it cannot.
func wholeFile(path string) (w window, ok bool) {
	w.models = map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return window{}, false
	}
	defer func() { _ = f.Close() }() // opened read-only: a close error loses nothing
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxTranscriptLine)
	for sc.Scan() && len(w.models) < 2 {
		if l, ok := assistantLine(sc.Bytes()); ok {
			w.count(l)
		}
	}
	return w, sc.Err() == nil
}

// window is what a transcript's assistant lines since a time say: their count, their models,
// and whether an older one was met.
type window struct {
	lines  int
	models map[string]bool
	older  bool
}

// scanModels reads buf's assistant lines from the end back to the first one older than since.
// partial says buf's first line may be cut.
func scanModels(buf []byte, partial bool, since time.Time) window {
	w := window{models: map[string]bool{}}
	lines := bytes.Split(buf, []byte("\n"))
	for i, line := range slices.Backward(lines) {
		if partial && i == 0 {
			break
		}
		l, ok := assistantLine(line)
		if !ok {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, l.Timestamp); err == nil && ts.Before(since) {
			w.older = true
			return w
		}
		w.count(l)
		if len(w.models) > 1 {
			return w // two models already: the rest cannot change the answer
		}
	}
	return w
}

// assistantLine is the line read as a transcript line, when it may be an assistant message.
func assistantLine(line []byte) (l transcriptLine, ok bool) {
	if !bytes.Contains(line, []byte(`"assistant"`)) || json.Unmarshal(line, &l) != nil {
		return transcriptLine{}, false
	}
	return l, true
}

// count adds one line inside the window, and its model when it is an assistant message's.
func (w *window) count(l transcriptLine) {
	w.lines++
	if l.Type == "assistant" && l.Message.Model != "" && l.Message.Model != syntheticModel {
		w.models[l.Message.Model] = true
	}
}

// tail is the transcript's last transcriptTail bytes and the offset they start at; nil when it
// cannot be read.
func tail(path string) (buf []byte, start int64) { return tailOf(path, transcriptTail) }

// tailOf is the transcript's last size bytes and the offset they start at; nil when it cannot
// be read.
func tailOf(path string, size int64) (buf []byte, start int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer func() { _ = f.Close() }() // opened read-only: a close error loses nothing
	st, err := f.Stat()
	if err != nil {
		return nil, 0
	}
	start = max(st.Size()-size, 0)
	buf = make([]byte, st.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, 0
	}
	return buf, start
}
