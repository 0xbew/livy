package doctor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/ccpolicy"
	"github.com/0xbew/livy/internal/ledger"
)

// timingHooks are the Claude Code events whose tool.call hook times every tool call, the
// timing Livy falls back on when Claude Code's OTEL goes elsewhere.
var timingHooks = map[string]string{"PostToolUse": "hook tool.call", "PostToolUseFailure": "hook tool.call"}

// everyTool is the matcher subject for timingHooks: "*" passes only a matcher that lets every
// tool through ("", "*" or a regex such as ".*"), since a hook on some tools times only those.
var everyTool = map[string]string{"PostToolUse": "*", "PostToolUseFailure": "*"}

// quietTurns is how many Claude Code turns may wait locally with no tool.call line before the
// hook counts as not recording: a turn can end without a tool call, several in a row rarely do.
const quietTurns = 3

// hookTiming is whether Livy's own hooks time Claude Code's tool calls: installed for every
// tool, not blocked by a settings layer, and recording. ok false says why not; ok true says
// what shows they are.
func hookTiming(m ccpolicy.Settings, settingsPath, dir string) (ok bool, detail string) {
	if blocks := m.HookBlocks(); len(blocks) > 0 {
		return false, blocks[0].Key + " in Claude Code's managed settings keeps Livy's hooks from running"
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return false, fmt.Sprintf("%s unreadable (%v), so whether Livy's tool.call hook is installed is unknown", settingsPath, err)
	}
	var doc struct {
		Hooks   map[string][]hookGroup `json:"hooks"`
		NoHooks bool                   `json:"disableAllHooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false, fmt.Sprintf("%s is not the settings shape Livy reads (%v)", settingsPath, err)
	}
	if doc.NoHooks && m.NoHooks == nil {
		return false, "disableAllHooks in Claude Code's user settings keeps Livy's hooks from running"
	}
	if missing, _ := livyHooks(doc.Hooks, timingHooks, everyTool); len(missing) > 0 {
		return false, fmt.Sprintf("no Livy tool.call hook for every tool on %s in %s; run `livy install`", strings.Join(missing, ", "), settingsPath)
	}
	w := waitingClaudeLines(dir)
	switch {
	case w.calls > 0:
		return true, fmt.Sprintf("the tool.call hook is installed and recording: %d tool.call line(s) waiting to ship, newest %s", w.calls, w.newestCall)
	case w.unread:
		return true, "the tool.call hook is installed; a waiting ledger file could not be read to its end, so whether it records is unknown"
	case w.turns >= quietTurns:
		return false, fmt.Sprintf("the tool.call hook is installed but recorded nothing: %d Claude Code turn(s) waiting to ship since %s and no tool.call line",
			w.turns, w.firstTurn)
	}
	return true, fmt.Sprintf("the tool.call hook is installed; fewer than %d Claude Code turns are waiting to ship, too few to judge whether it records", quietTurns)
}

// waiting is what Claude Code's hooks wrote that has not shipped yet.
type waiting struct {
	turns, calls          int
	firstTurn, newestCall string
	unread                bool            // a file or folder not read to its end: a missing tool.call proves nothing
	seen                  map[string]bool // lines counted, so one in the ledger and in a segment counts once
}

// waitingClaudeLines reads the live ledger and the pending segments, the only lines still on
// the machine, for Claude Code's turn.ended and tool.call lines (Codex's carry a harness).
// The ledger is read before the segments are listed, so a rotation in between shows its lines
// twice, counted once, rather than missing them.
func waitingClaudeLines(dir string) waiting {
	w := waiting{seen: map[string]bool{}}
	w.add(filepath.Join(dir, ledger.FileName))
	segments := filepath.Join(dir, ledger.SegmentsDir)
	_ = filepath.WalkDir(segments, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			w.unread = w.unread || p != segments || !errors.Is(err, fs.ErrNotExist)
		case !d.IsDir() && strings.HasSuffix(p, ".jsonl"):
			w.add(p)
		}
		return nil
	})
	return w
}

func (w *waiting) add(path string) {
	f, err := os.Open(path)
	if err != nil {
		w.unread = w.unread || !errors.Is(err, fs.ErrNotExist)
		return
	}
	defer func() { _ = f.Close() }() // read-only: a close error loses nothing
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		w.count(sc.Bytes())
	}
	if sc.Err() != nil {
		w.unread = true
	}
}

// count adds one ledger line when it is a Claude Code turn.ended or tool.call not counted yet.
func (w *waiting) count(line []byte) {
	var rec struct {
		Event   string `json:"event"`
		TS      string `json:"ts"`
		Harness string `json:"harness"`
	}
	if json.Unmarshal(line, &rec) != nil || (rec.Harness != "" && rec.Harness != "claude-code") ||
		(rec.Event != "turn.ended" && rec.Event != "tool.call") || w.seen[string(line)] {
		return
	}
	w.seen[string(line)] = true
	if rec.Event == "turn.ended" {
		w.turns++
		if w.firstTurn == "" || earlier(rec.TS, w.firstTurn) {
			w.firstTurn = rec.TS
		}
		return
	}
	w.calls++
	if w.newestCall == "" || earlier(w.newestCall, rec.TS) {
		w.newestCall = rec.TS
	}
}

// earlier says a is before b; an unreadable time is never earlier.
func earlier(a, b string) bool {
	ta, errA := time.Parse(time.RFC3339, a)
	tb, errB := time.Parse(time.RFC3339, b)
	return errA == nil && errB == nil && ta.Before(tb)
}
