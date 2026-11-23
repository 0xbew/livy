package doctor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/emit"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/ledger"
)

// StatusFileName is the daily check's result, in ~/.livy, where the schedule writes it
// and `doctor --errors` reads it.
const StatusFileName = "doctor-status.json"

var emailRE = regexp.MustCompile(`[A-Za-z0-9._%+-]+@([A-Za-z0-9.-]+)`)

// redact takes the home folder and email local parts out of text that gets pasted around.
func redact(text string) string {
	if home := hook.Home(); home != "" && home != "/" {
		text = strings.ReplaceAll(text, home, "~")
	}
	return emailRE.ReplaceAllString(text, "***@$1")
}

// notify tells the engineer something is wrong, without blocking: a failure the unattended
// daily job only writes to a file is one nobody reads. macOS only.
func notify(ctx context.Context, summary string) {
	if runtime.GOOS != "darwin" {
		return
	}
	b, _ := json.Marshal(summary)
	script := "display notification " + string(b) + ` with title "livy" subtitle "local capture needs attention"`
	cctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	// A notification that cannot be shown changes nothing: the status file already holds the failure.
	_ = exec.CommandContext(cctx, "osascript", "-e", script).Run()
}

type errRow struct {
	ts, where, reason string
	key               string // the timestamp in UTC, so offsets order correctly; "" sorts first
	order             int
}

// ShowErrors is `doctor --errors`: every ledger.error in the live ledger, the waiting
// segments and the rejected ones, the newest `keep` by timestamp, and what the daily check
// last found. It exits 1 when there is anything to read: it answers "did capture ever break
// here", where plain doctor answers "is it broken now".
func ShowErrors(keep int, stdout, stderr io.Writer) int {
	dir := hook.DataDir()
	sc := &errScan{stderr: stderr}
	for _, path := range sc.sources(dir) {
		sc.file(path, dir)
	}
	rows, total, unreadable := sc.rows, sc.total, sc.unreadable
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].key != rows[j].key {
			return rows[i].key < rows[j].key
		}
		return rows[i].order < rows[j].order
	})
	if len(rows) > keep {
		rows = rows[len(rows)-keep:]
	}
	if total > 0 {
		fmt.Fprintf(stdout, "%d capture failure(s) recorded (live ledger + segments waiting to ship)\n", total)
	} else {
		fmt.Fprintln(stdout, "no capture failures recorded")
	}
	if total > len(rows) {
		fmt.Fprintf(stdout, "  showing the %d newest — pass --errors-keep %d for all of them\n", len(rows), total)
	}
	for _, r := range rows {
		fmt.Fprintf(stdout, "  %s  [%s]  %s\n", r.ts, r.where, r.reason)
	}
	if unreadable > 0 {
		fmt.Fprintf(stdout, "  (%d line(s) or file(s) could not be read — this list is incomplete)\n", unreadable)
	}
	return lastCheck(stdout, total > 0 || unreadable > 0)
}

// errScan collects the ledger.error rows of every file it reads, and counts what it could not.
type errScan struct {
	stderr     io.Writer
	rows       []errRow
	total      int
	unreadable int
}

// sources is the live ledger, then every segment waiting, rejected or spilled, each folder
// sorted by name.
func (sc *errScan) sources(dir string) []string {
	sources := []string{filepath.Join(dir, ledger.FileName)}
	for _, sub := range []string{emit.SegmentsDir, emit.RejectedDir, ledger.SpillDir} {
		var found []string
		root := filepath.Join(dir, sub)
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil && errors.Is(err, fs.ErrNotExist) && p == root:
				// no such folder: nothing was written there
			case err != nil:
				sc.unreadable++
				fmt.Fprintf(sc.stderr, "doctor: could not read %s: %v\n", p, err)
			case !d.IsDir() && strings.HasSuffix(p, ".jsonl"):
				found = append(found, p)
			}
			return nil
		})
		sort.Strings(found)
		sources = append(sources, found...)
	}
	return sources
}

// file adds the ledger.error lines of one file.
func (sc *errScan) file(path, dir string) {
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			sc.unreadable++
			fmt.Fprintf(sc.stderr, "doctor: could not read %s: %v\n", filepath.Base(path), err)
		}
		return
	}
	where := errSource(path, dir)
	lines := bufio.NewScanner(f)
	lines.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for lines.Scan() {
		sc.line(strings.TrimSpace(lines.Text()), where)
	}
	if lines.Err() != nil {
		sc.unreadable++
	}
	_ = f.Close()
}

// line adds one ledger line when it is a ledger.error.
func (sc *errScan) line(line, where string) {
	if line == "" {
		sc.unreadable++ // a blank line is a record the writer must never produce
		return
	}
	var rec struct {
		Event  string `json:"event"`
		TS     string `json:"ts"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal([]byte(line), &rec) != nil {
		sc.unreadable++
		return
	}
	if rec.Event != "ledger.error" {
		return
	}
	sc.total++
	norm := ""
	if t, err := time.Parse(time.RFC3339, rec.TS); err == nil {
		norm = t.UTC().Format("2006-01-02T15:04:05.000000000Z")
	} else {
		sc.unreadable++
	}
	sc.rows = append(sc.rows, errRow{ts: rec.TS, where: where, reason: rec.Reason, key: norm, order: sc.total})
}

// lastCheck prints what the daily check last found, and returns the exit code: 1 when it
// failed, could not be read, or found is true.
func lastCheck(stdout io.Writer, found bool) int {
	status := filepath.Join(hook.Home(), ".livy", StatusFileName)
	raw, err := os.ReadFile(status)
	if os.IsNotExist(err) {
		fmt.Fprintln(stdout, "\nthe daily check has not run yet (it runs every 24h)")
		if found {
			return 1
		}
		return 0
	}
	var last struct {
		CheckedAt string         `json:"checked_at"`
		OK        bool           `json:"ok"`
		Failures  []statusDetail `json:"failures"`
		Warnings  []statusDetail `json:"warnings"`
	}
	if err != nil || json.Unmarshal(raw, &last) != nil {
		fmt.Fprintln(stdout, "\nlast unattended check: unreadable")
		return 1
	}
	fmt.Fprintf(stdout, "\nlast unattended check: %s  ok=%t\n", last.CheckedAt, last.OK)
	for _, f := range last.Failures {
		fmt.Fprintf(stdout, "  FAIL %s: %s\n", f.Check, clip(f.Detail, 110))
	}
	for _, w := range last.Warnings {
		fmt.Fprintf(stdout, "  warn %s: %s\n", w.Check, clip(w.Detail, 110))
	}
	if !last.OK || found {
		return 1
	}
	return 0
}

type statusDetail struct {
	Check  string `json:"check"`
	Status string `json:"status,omitempty"`
	Detail string `json:"detail"`
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// errSource says whether a failure will ship in the next pass (live, segments) or is gone
// for good (rejected), from the path below the data folder.
func errSource(path, dir string) string {
	if path == filepath.Join(dir, ledger.FileName) {
		return "live"
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return "unknown"
	}
	head := strings.Split(rel, string(filepath.Separator))[0]
	if head == emit.SegmentsDir || head == emit.RejectedDir || head == ledger.SpillDir {
		return head
	}
	return "unknown"
}
