// Package vault keeps the text of each captured prompt on this machine for KeepDays, so it
// outlives the harness's transcript cleanup. The text never leaves the machine from here:
// `livy show` is the only reader, and it scrubs before it prints.
//
// One JSON line per prompt, in one file per UTC day of the prompt, owner-only. A day's file is
// deleted once its newest possible prompt is older than KeepDays.
package vault

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/ledger"
)

// Dir is the vault's folder inside the data folder.
const Dir = "prompt-vault"

// KeepDays is how long a prompt's text is kept, counted from when it was sent.
const KeepDays = 90

const (
	keep          = KeepDays * 24 * time.Hour
	day           = 24 * time.Hour
	dayLayout     = "2006-01-02"
	fileSuffix    = ".jsonl"
	schemaVersion = 1
	// maxLine is the longest line read back: a pasted prompt can be large.
	maxLine = 64 << 20
)

// Record is one kept prompt.
type Record struct {
	Schema int `json:"schema"`
	// PromptID is prompt capture's id ("prompt:<transcript record uuid>"), as prompt.captured carries it.
	PromptID string `json:"prompt_id"`
	// HarnessPromptID is the harness's own prompt id (Claude Code's promptId), as the
	// UserPromptSubmit hook's prompt.submitted carries it; "" when the harness records none.
	HarnessPromptID string    `json:"harness_prompt_id,omitempty"`
	SessionID       string    `json:"session_id"`
	Repo            string    `json:"repo"`
	Branch          string    `json:"branch"`
	TS              time.Time `json:"ts"`
	Text            string    `json:"text"`
}

// Kept is what one Keep call did with the records it was given.
type Kept struct {
	Kept    int // written
	Already int // in the vault before
	Empty   int // no id, no time or no text: never written
	TooOld  int // sent longer ago than KeepDays
	// Unchecked is why a day file's last line could not be checked for a torn write, one per
	// day file; its records were kept without the check.
	Unchecked []error
}

// Keep writes each record not yet in the vault under dataDir. A record with no id, time or
// text is never written: a reveal would show it as the prompt.
func Keep(dataDir string, recs []Record, now time.Time) (Kept, error) {
	var k Kept
	byDay := map[string][]Record{}
	for _, r := range recs {
		switch {
		case r.PromptID == "" || r.TS.IsZero() || strings.TrimSpace(r.Text) == "":
			k.Empty++
		case now.Sub(r.TS) > keep:
			k.TooOld++
		default:
			r.Schema, r.TS = schemaVersion, r.TS.UTC()
			name := r.TS.Format(dayLayout) + fileSuffix
			byDay[name] = append(byDay[name], r)
		}
	}
	if len(byDay) == 0 {
		return k, nil
	}
	dir, err := ownerOnlyDir(dataDir)
	if err != nil {
		return k, err
	}
	var errs []error
	for name, day := range byDay {
		path := filepath.Join(dir, name)
		have, err := idsIn(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var fresh []Record
		for _, r := range day {
			if have[r.PromptID] {
				k.Already++
				continue
			}
			have[r.PromptID] = true
			fresh = append(fresh, r)
		}
		unchecked, err := appendRecords(path, fresh)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if unchecked != nil {
			k.Unchecked = append(k.Unchecked, unchecked)
		}
		k.Kept += len(fresh)
	}
	return k, errors.Join(errs...)
}

// ownerOnlyDir makes the vault folder, and makes it owner-only if it was not.
func ownerOnlyDir(dataDir string) (string, error) {
	dir := filepath.Join(dataDir, Dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("vault: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("vault: %w", err)
	}
	return dir, nil
}

// appendRecords appends recs to one day file in one write; a write that fails partway is cut
// back to where it started, so the next record never lands glued to half of this one.
// unchecked is why the file's last line could not be checked for a torn write: the records
// were then written without it.
func appendRecords(path string, recs []Record) (unchecked, err error) {
	if len(recs) == 0 {
		return nil, nil
	}
	var buf []byte
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			return nil, fmt.Errorf("vault: %w", err)
		}
		buf = append(append(buf, line...), '\n')
	}
	t, err := ledger.OpenTail(path)
	if err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	defer func() {
		if cerr := t.File.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("vault: %w", cerr)
		}
	}()
	if err := t.File.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	if _, err := t.File.Write(t.Lead(buf)); err != nil {
		t.CutBack()
		return nil, fmt.Errorf("vault: %w", err)
	}
	return t.Unchecked, nil
}

// idsIn is the prompt ids already in one day file; none when it does not exist yet.
func idsIn(path string) (map[string]bool, error) {
	ids := map[string]bool{}
	err := eachRecord(path, func(r Record) { ids[r.PromptID] = true })
	return ids, err
}

// eachRecord calls f on every readable record in one day file. A line that does not decode
// is skipped: one torn line must not hide the rest of the day.
func eachRecord(path string, f func(Record)) error {
	fh, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	defer func() { _ = fh.Close() }() // opened read-only: a close error loses nothing
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.PromptID != "" {
			f(r)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("vault: %s: %w", filepath.Base(path), err)
	}
	return nil
}

// dayFiles is the vault's day files with the UTC day each one holds, oldest first. A file whose
// name is not a day is not the vault's and is left alone.
func dayFiles(dataDir string) ([]string, []time.Time, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, Dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("vault: %w", err)
	}
	var paths []string
	var days []time.Time
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), fileSuffix)
		d, err := time.Parse(dayLayout, name)
		if !ok || err != nil || e.IsDir() {
			continue
		}
		paths, days = append(paths, filepath.Join(dataDir, Dir, e.Name())), append(days, d)
	}
	return paths, days, nil
}

// Prune deletes the day files whose prompts are all older than KeepDays, and returns how many.
func Prune(dataDir string, now time.Time) (int, error) {
	paths, days, err := dayFiles(dataDir)
	if err != nil {
		return 0, err
	}
	removed := 0
	var errs []error
	for i, path := range paths {
		if now.Sub(days[i].Add(day)) < keep {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("vault: %w", err))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// Find is the kept records whose prompt id or harness prompt id is in ids, in the order of ids,
// each record once.
func Find(dataDir string, ids []string) ([]Record, error) {
	paths, _, err := dayFiles(dataDir)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	byID := map[string]Record{}
	var errs []error
	for _, path := range paths {
		err := eachRecord(path, func(r Record) {
			if want[r.PromptID] {
				byID[r.PromptID] = r
			}
			if r.HarnessPromptID != "" && want[r.HarnessPromptID] {
				byID[r.HarnessPromptID] = r
			}
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	var out []Record
	var seen []string
	for _, id := range ids {
		r, ok := byID[id]
		if !ok || slices.Contains(seen, r.PromptID) {
			continue
		}
		seen = append(seen, r.PromptID)
		out = append(out, r)
	}
	return out, errors.Join(errs...)
}
