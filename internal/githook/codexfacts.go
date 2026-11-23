package githook

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/0xbew/livy/internal/extract/codex"
)

// A Codex session's rollouts outlive its ledger lines, as Claude Code's transcripts do: the
// main thread's is named by the session id, and each subagent's has its own id with a
// session_meta naming the session. Each turn starts with a turn_context naming its model;
// the main thread's newest turn is the prompt a commit answers.

// codexRolloutRecord is the part of a rollout line these readers use.
type codexRolloutRecord struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		SessionID  string `json:"session_id"`
		ID         string `json:"id"`
		CLIVersion string `json:"cli_version"`
		TurnID     string `json:"turn_id"`
		Model      string `json:"model"`
	} `json:"payload"`
}

// threadID is the shape of a Codex thread id.
var threadID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// codexMainRollout is the session's own rollout, or "" when there is none.
func codexMainRollout(session string) string {
	if !threadID.MatchString(session) {
		return "" // spliced into a glob: only a plain id
	}
	found, _ := filepath.Glob(filepath.Join(codex.Root(), "*", "*", "*", "rollout-*-"+session+".jsonl"))
	if len(found) == 0 {
		return ""
	}
	return found[len(found)-1]
}

// codexRolloutFacts adds the model of each of the session's turns since `since`, in its own
// rollout and each subagent's, and returns the Codex version its rollout names. A turn whose
// model cannot be read adds "", which never agrees with a real model. A rollout written since
// `since` also adds the turn it was in at `since`: a second commit in one turn has no
// turn_context after the first.
func codexRolloutFacts(session string, since time.Time, models map[string]bool) (version string) {
	main := codexMainRollout(session)
	if main == "" {
		return ""
	}
	meta, ok := firstRecord(main)
	if !ok {
		return ""
	}
	version = meta.Payload.CLIVersion
	paths := []string{main}
	if started, err := time.Parse(time.RFC3339, meta.Timestamp); err == nil {
		paths = append(paths, codexSubagentRollouts(session, main, started)...)
	}
	for _, path := range paths {
		if !active(path, since) {
			continue
		}
		var current *string // the turn in effect at since
		read := codexTurns(path, func(r codexRolloutRecord) {
			ts, err := time.Parse(time.RFC3339, r.Timestamp)
			switch {
			case err != nil:
				models[""] = true // a turn of unknown time may be in the window
			case !ts.Before(since):
				models[r.Payload.Model] = true
			default:
				current = &r.Payload.Model
			}
		})
		if current != nil {
			models[*current] = true
		}
		if !read {
			models[""] = true // turns past the unread line are unknown
		}
	}
	return version
}

// codexSubagentRollouts is the rollouts written since the session started whose session_meta
// names the session as their own.
func codexSubagentRollouts(session, main string, started time.Time) []string {
	all, _ := filepath.Glob(filepath.Join(codex.Root(), "*", "*", "*", "rollout-*.jsonl"))
	var subs []string
	for _, path := range all {
		if path == main || !active(path, started) {
			continue
		}
		if meta, ok := firstRecord(path); ok && meta.Payload.SessionID == session && meta.Payload.ID != session {
			subs = append(subs, path)
		}
	}
	return subs
}

// codexPrompt is the turn id of the main thread's newest turn, or "".
func codexPrompt(session string) (id string) {
	main := codexMainRollout(session)
	if main == "" {
		return ""
	}
	if !codexTurns(main, func(r codexRolloutRecord) { id = r.Payload.TurnID }) {
		return "" // a newer turn may be past the unread line
	}
	return id
}

// firstRecord is the rollout's session_meta, its first line.
func firstRecord(path string) (codexRolloutRecord, bool) {
	f, err := os.Open(path)
	if err != nil {
		return codexRolloutRecord{}, false
	}
	defer func() { _ = f.Close() }() // opened read-only: a close error loses nothing
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxTranscriptLine)
	if !sc.Scan() {
		return codexRolloutRecord{}, false
	}
	var r codexRolloutRecord
	if json.Unmarshal(sc.Bytes(), &r) != nil || r.Type != "session_meta" {
		return codexRolloutRecord{}, false
	}
	return r, true
}

// codexTurns calls each with every turn_context record in the rollout, oldest first; false
// when the rollout could not be read to its end, a line that names a turn_context does not
// parse, or the last line does not parse.
func codexTurns(path string, each func(codexRolloutRecord)) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }() // opened read-only: a close error loses nothing
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxTranscriptLine)
	var last []byte
	for sc.Scan() {
		if !bytes.Contains(sc.Bytes(), []byte(`"turn_context"`)) {
			last = append(last[:0], sc.Bytes()...)
			continue
		}
		last = last[:0]
		var r codexRolloutRecord
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			return false // a torn line may be a turn whose model and id are then unknown
		}
		if r.Type == "turn_context" {
			each(r)
		}
	}
	if len(last) > 0 && !json.Valid(last) {
		return false // Codex is appending: a tail cut before its type may be a turn_context
	}
	return sc.Err() == nil
}
