// Package tooltarget keeps a short target for each tool call on this machine only: the file a
// read or edit touched, the program a shell command ran, the host a fetch reached, a subagent's
// task label. Never an argument's value and never a tool's output. Every target is scrubbed and
// audited with the denylist before it is written, and dropped when the audit refuses it or no
// denylist is there to audit with.
//
// One JSON line per call, in one file per UTC day, owner-only, under Dir in the data folder,
// which nothing ships: `livy pr` and `livy profile` read it to label a trace's spans. A day's
// file is deleted once it is older than KeepDays.
package tooltarget

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
	"unicode/utf8"

	"github.com/0xbew/scrub"
)

// Dir is the folder the targets are kept in, inside the data folder.
const Dir = "tool-targets"

// KeepDays is how long a target is kept, counted from its day.
const KeepDays = 90

const (
	schemaVersion = 1
	dayLayout     = "2006-01-02"
	fileSuffix    = ".jsonl"
	// MaxRunes is the longest target kept, after scrubbing: a label or a path, not text.
	MaxRunes = 120
	// maxLine is the longest line read back.
	maxLine = 64 << 10
)

// Record is one kept target.
type Record struct {
	Schema    int       `json:"schema"`
	TS        time.Time `json:"ts"`
	SessionID string    `json:"session_id"`
	ToolUseID string    `json:"tool_use_id"`
	Tool      string    `json:"tool"`
	Target    string    `json:"target"`
}

// DataNames are the folders this package keeps in the data folder: `livy uninstall --purge`
// deletes them.
func DataNames() []string { return []string{Dir} }

// ErrNoDenylist is a target that cannot be audited: without the denylist's names nothing is kept.
var ErrNoDenylist = errors.New("no usable denylist: tool targets are not kept")

// Clean is target scrubbed with d, cut to MaxRunes, or "" when the audit still finds a name or a
// secret in it.
func Clean(target string, d *scrub.Denylist) (string, error) {
	if d == nil {
		return "", ErrNoDenylist
	}
	out := strings.TrimSpace(scrub.Scrub(target, d, 2*utf8.RuneCountInString(target)+100))
	if r := []rune(out); len(r) > MaxRunes {
		out = string(r[:MaxRunes])
	}
	if len(scrub.Audit(out, d)) > 0 {
		return "", nil
	}
	return out, nil
}

// Append writes rec to its day's file under dataDir.
func Append(dataDir string, rec Record) error {
	dir := filepath.Join(dataDir, Dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("make %s: %w", Dir, err)
	}
	rec.Schema, rec.TS = schemaVersion, rec.TS.UTC()
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode a target: %w", err)
	}
	path := filepath.Join(dir, rec.TS.Format(dayLayout)+fileSuffix)
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	_, werr := fh.Write(append(line, '\n'))
	if cerr := fh.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), werr)
	}
	return nil
}

// Prune deletes the day files under dataDir older than KeepDays; a store not made yet has none.
func Prune(dataDir string, now time.Time) error {
	dir := filepath.Join(dataDir, Dir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list %s: %w", Dir, err)
	}
	cutoff := now.UTC().AddDate(0, 0, -KeepDays)
	var errs []error
	for _, e := range entries {
		day, err := time.Parse(dayLayout, strings.TrimSuffix(e.Name(), fileSuffix))
		if err != nil || !strings.HasSuffix(e.Name(), fileSuffix) || !day.AddDate(0, 0, 1).Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Read is the targets kept under dataDir for the given sessions, by tool use id. A missing folder
// is no targets; an unreadable file or line is counted in bad and skipped.
func Read(dataDir string, sessions map[string]bool) (targets map[string]string, bad int) {
	targets = map[string]string{}
	paths, _ := filepath.Glob(filepath.Join(dataDir, Dir, "*"+fileSuffix))
	for _, p := range paths {
		bad += readFile(p, sessions, targets)
	}
	return targets, bad
}

// readFile adds one day file's targets for sessions to into, and returns how many of its lines,
// or the file itself, could not be read.
func readFile(path string, sessions map[string]bool, into map[string]string) (bad int) {
	fh, err := os.Open(path)
	if err != nil {
		return 1
	}
	defer func() { _ = fh.Close() }() // opened read-only: a close error loses nothing
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 4096), maxLine)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil || r.ToolUseID == "" {
			bad++
			continue
		}
		if sessions[r.SessionID] && r.Target != "" {
			into[r.ToolUseID] = r.Target
		}
	}
	if sc.Err() != nil {
		bad++
	}
	return bad
}
