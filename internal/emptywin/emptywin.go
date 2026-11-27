// Package emptywin says why a commit's session window has no model call in the ledger, so a
// report can name the case instead of showing a page of zeros. It reads the session's local
// transcripts (timestamps and byte offsets only, never their text) and the extractor's state.
package emptywin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/extract/claudecode"
	"github.com/0xbew/livy/internal/why"
)

// Kinds, as a report's empty.kind says them.
const (
	// KindNotRead is a window whose calls are in a transcript the extractor has not read that far.
	KindNotRead = "not_extracted"
	// KindOutOfScope is a window whose calls were read but worked outside every in-scope repo.
	KindOutOfScope = "out_of_scope"
	// KindNoCalls is a window in which the session made no model call at all.
	KindNoCalls = "no_calls"
	// KindNoTranscript is a Claude Code session with no transcript on this machine.
	KindNoTranscript = "no_transcript"
	// KindHarness is a session of a harness whose usage Livy reads another way, or not at all.
	KindHarness = "harness"
	// KindUnreadable is a window whose transcript could not be read in full: its cause is unknown.
	KindUnreadable = "unreadable"
)

const (
	harnessClaude  = "claude-code"
	syntheticModel = "<synthetic>"
)

// sessionID is the shape of a session id Livy looks a transcript up by.
var sessionID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// Finding is why one window is empty: a fixed kind for code and a sentence for people.
type Finding struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// Explainer explains empty windows against one data folder and the transcript roots.
type Explainer struct {
	Roots    []string
	progress claudecode.Progress
	read     map[string]fileCalls // each transcript's calls, read once
}

// New is an Explainer reading the extractor's state in dataDir and transcripts under roots.
func New(dataDir string, roots []string) *Explainer {
	return &Explainer{Roots: roots, progress: claudecode.LoadProgress(dataDir), read: map[string]fileCalls{}}
}

// Explain says why session s's window has no model call line.
func (x *Explainer) Explain(s why.Session) Finding {
	switch s.Harness {
	case "", harnessClaude:
	case "codex":
		return Finding{KindHarness, "a Codex session: Livy has no usage line for it in this window (it reads Codex usage only from the rollout's token counts)"}
	default:
		return Finding{KindHarness, fmt.Sprintf("a %s session: Livy has no usage line for it in this window", s.Harness)}
	}
	var files []transcript
	if sessionID.MatchString(s.ID) { // a trailer's text, never a glob or a path
		files = x.transcripts(s.ID)
	}
	if len(files) == 0 {
		return Finding{KindNoTranscript, "no transcript of this session on this machine: it ran on another machine, or in an app whose transcripts Livy does not read"}
	}
	calls, unread, whole := 0, 0, true
	for _, f := range files {
		through, read := x.progress.Through(f.root, f.path)
		got, ok := x.callsOf(f.path)
		whole = whole && ok
		for _, c := range got { // a call copied into two files counts twice
			if (!s.From.IsZero() && !c.at.After(s.From)) || c.at.After(s.To) {
				continue
			}
			calls++
			if !read || c.offset >= through {
				unread++
			}
		}
	}
	switch {
	case unread > 0:
		return Finding{KindNotRead, fmt.Sprintf("%d of this session's model calls in this window have not been read from its transcript yet: run `livy extract` (if they stay unread, `livy doctor` names a transcript held at a format change)", unread)}
	case calls > 0:
		return Finding{KindOutOfScope, fmt.Sprintf("this session's %d model calls in this window worked outside every in-scope repo (its folder and the folders its calls named), so none was recorded", calls)}
	}
	if !whole { // the calls counted prove the cases above; only "none" needs every record
		return Finding{KindUnreadable, "a transcript of this session could not be read in full, so why this window has no model call is unknown"}
	}
	return Finding{KindNoCalls, "the session made no model call in this window: the commit came from a hook, a script or a command the model did not run, or right after the session's previous commit"}
}

// transcript is one transcript file and the root it was found under.
type transcript struct{ root, path string }

// transcripts are session id's transcript and its subagents', under every root.
func (x *Explainer) transcripts(id string) []transcript {
	var out []transcript
	for _, root := range x.Roots {
		mains, _ := filepath.Glob(filepath.Join(root, "*", id+".jsonl"))
		for _, m := range mains {
			out = append(out, transcript{root, m})
			sub := filepath.Join(strings.TrimSuffix(m, ".jsonl"), "subagents")
			_ = filepath.WalkDir(sub, func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
					out = append(out, transcript{root, p})
				}
				return nil // a folder that cannot be read holds nothing this can count
			})
		}
	}
	return out
}

// call is one model call in a transcript: when its last record was written, and where.
type call struct {
	at     time.Time
	offset int64
}

// callsOf is every model call in the transcript at path, read once per Explainer; false when the
// file could not be read to its end.
func (x *Explainer) callsOf(path string) ([]call, bool) {
	if c, ok := x.read[path]; ok {
		return c.calls, c.ok
	}
	c := fileCalls{}
	c.calls, c.ok = readCalls(path)
	if x.read == nil {
		x.read = map[string]fileCalls{}
	}
	x.read[path] = c
	return c.calls, c.ok
}

// fileCalls is one transcript's calls, and whether it was read to its end.
type fileCalls struct {
	calls []call
	ok    bool
}

// readCalls is every model call in the transcript at path, by its last record; false when the file
// could not be read to its end.
func readCalls(path string) ([]call, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }() // read-only
	last := map[string]call{}
	var order []string
	var readErr error
	var off int64
	r := bufio.NewReaderSize(f, 1<<20)
	marker := []byte(`"type":"assistant"`)
	for {
		line, err := r.ReadBytes('\n')
		if bytes.Contains(line, marker) {
			if id, at, ok := callIn(line); ok {
				if _, seen := last[id]; !seen {
					order = append(order, id)
				}
				last[id] = call{at, off}
			}
		}
		off += int64(len(line))
		if err != nil {
			readErr = err
			break
		}
	}
	out := make([]call, 0, len(order))
	for _, id := range order {
		out = append(out, last[id])
	}
	return out, errors.Is(readErr, io.EOF)
}

// callIn is the API message id and time of one transcript line that is a model call's record.
func callIn(line []byte) (string, time.Time, bool) {
	var rec struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			ID    string          `json:"id"`
			Model string          `json:"model"`
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" || rec.Message.ID == "" || rec.Message.Usage == nil ||
		rec.Message.Model == syntheticModel {
		return "", time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	return rec.Message.ID, at, err == nil
}
