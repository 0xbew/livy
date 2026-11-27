package extract

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/clock"
)

// WrittenName is the log of lines already written, so a pass that is killed (or whose later
// batch fails) before its offsets are saved does not append the same lines again on the
// next run, and a call copied into a second transcript is not written twice across runs.
// One line per ledger line written: "<UTC day> <16 hex of SHA-256(event key)>\n", 28 bytes.
// Entries older than WrittenDays are dropped at the end of a run, so the file holds about
// WrittenDays of lines: some 6 MB at 7,000 lines a day.
const (
	WrittenName = "extract-written.log"
	WrittenDays = 30
)

// DataNames are the files and folders this package keeps in the data folder, by exact name.
// `livy uninstall --purge` deletes these and nothing else.
func DataNames() []string { return []string{WrittenName} }

// Written is the set of lines already written in the last WrittenDays.
type Written struct {
	path   string
	set    map[string]bool
	stale  bool // the file holds entries past the window, or a broken line: rewrite it at Compact
	broken bool // the file could not be read to its end: never rewrite it from a partial read
	needNL bool // the file ends in a half line from a killed append: start the next append on a new line
	keep   []string
}

// LoadWritten reads the log; a missing log is an empty set. A log that cannot be read is
// an error: the caller says so, since without it a replay can append lines again.
func LoadWritten(dir string) (*Written, error) {
	w := &Written{path: filepath.Join(dir, WrittenName), set: map[string]bool{}}
	f, err := os.Open(w.path)
	if errors.Is(err, os.ErrNotExist) {
		return w, nil
	}
	if err != nil {
		return w, err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, st.Size()-1); err == nil && last[0] != '\n' {
			w.needNL, w.stale = true, true
		}
	}
	cutoff := clock.Now().AddDate(0, 0, -WrittenDays).Format(time.DateOnly)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		day, key, ok := strings.Cut(sc.Text(), " ")
		if !ok || len(key) != 16 {
			w.stale = true // a half line from a killed append: dropped at the rewrite
			continue
		}
		if day < cutoff {
			w.stale = true
			continue
		}
		w.set[key] = true
		w.keep = append(w.keep, sc.Text())
	}
	if err := sc.Err(); err != nil {
		w.broken = true
		return w, err
	}
	return w, nil
}

// LineKey is the identity of a ledger line for the written log, "" for a line it does not
// track (ledger.error, and anything without its id).
func LineKey(line map[string]any) string {
	event := stringOf(line["event"])
	id := ""
	if event == "work.pr_linked" {
		if n, ok := line["pr_number"].(int64); ok && stringOf(line["pr_repository"]) != "" {
			id = stringOf(line["session_id"]) + "|" + stringOf(line["pr_repository"]) + "|" + strconv.FormatInt(n, 10)
		}
	} else if field := map[string]string{"usage.api_call": "message_id", "call.detail": "message_id",
		"interaction": "interaction_id", "tool.outcome": "tool_use_id", "skill.loaded": "tool_use_id"}[event]; field != "" {
		id = stringOf(line[field])
	}
	if id == "" {
		return ""
	}
	h := sha256.Sum256([]byte(event + "\x00" + id))
	return hex.EncodeToString(h[:])[:16]
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// Filter drops lines already written; skipped is how many.
func (w *Written) Filter(lines []map[string]any) (kept []map[string]any, skipped int) {
	for _, l := range lines {
		if k := LineKey(l); k != "" && w.set[k] {
			skipped++
			continue
		}
		kept = append(kept, l)
	}
	return kept, skipped
}

// Record appends the keys of lines just written.
func (w *Written) Record(lines []map[string]any) error {
	day := clock.Now().Format(time.DateOnly)
	var b strings.Builder
	if w.needNL {
		b.WriteString("\n")
	}
	for _, l := range lines {
		if k := LineKey(l); k != "" && !w.set[k] {
			w.set[k] = true
			entry := day + " " + k
			w.keep = append(w.keep, entry)
			b.WriteString(entry + "\n")
		}
	}
	if b.Len() == 0 || b.String() == "\n" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	w.needNL = false
	return f.Close()
}

// Compact rewrites the log without entries past the window, through one fixed temp name.
func (w *Written) Compact() error {
	if !w.stale || w.broken {
		return nil
	}
	tmp := w.path + ".tmp"
	body := ""
	if len(w.keep) > 0 {
		body = strings.Join(w.keep, "\n") + "\n"
	}
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, w.path)
}
