package emit

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/config"
	"github.com/0xbew/livy/internal/event"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/ledger"
)

// `livy drain` moves what machines with no cloud credential (a hosted Amp orb) pushed to a git
// sink repository into this install's own sink, under the keys emit gives the same bytes and
// with the same write-once rule. It is configured as
//
//	{"drain": {"source": {"type": "git", "remote": "https://github.com/owner/repo.git", "branch": "main"}}}
//
// and the daily job runs it when that is set. It never changes a line: the pushing machine
// already applied its scope and wrote its lines, and a changed line would change the key. The
// segment is gzipped again as emit gzips it, so the object is what emit would have written. What it
// checks is that each object is one emit could have written: its key is the one emit gives its
// bytes, and each line is a ledger event the schema allows. Anything else is refused whole,
// said once as a ledger.error, and tried again on each run, so a newer schema read by a newer
// build drains then.

// ErrNoDrainSource is an install with no drain.source: there is nothing to drain.
var ErrNoDrainSource = errors.New("no drain source configured (drain.source.type)")

// Names in the data folder.
const (
	drainCloneDir  = "drain-git"
	drainStateName = "drain-state.json"
)

// The bounds on one object. The repository holds what the pushing machine chose to push, and
// whoever drives an orb drives that machine, so an object is never read or inflated unbounded.
const (
	maxDrainObject  = 64 << 20
	maxDrainInflate = 512 << 20
	maxReported     = 5
)

// drainKey is an emit object key: the stream, the UTC day, the handle when there is one, and the
// first 16 hex of the segment's sha256.
var drainKey = regexp.MustCompile(`^(source=[a-z-]+)/dt=(\d{4}-\d{2}-\d{2})/(?:handle=([a-z0-9-]+)/)?[0-9a-f]{16}\.jsonl\.gz$`)

// drainState is what has been drained from one remote and branch: every file up to Commit,
// except those Refused, by key, with the reason last said.
type drainState struct {
	Remote  string            `json:"remote"`
	Branch  string            `json:"branch"`
	Commit  string            `json:"commit"`
	Refused map[string]string `json:"refused,omitempty"`
}

// drainCount is one run's outcome.
type drainCount struct{ copied, present, refused int }

// Drain is `livy drain`: one pass. Exit 0 when it finished (refused objects included, each said
// once as a ledger.error), 2 on arguments or when no drain source is set, 1 on any other
// failure, a drain.source that cannot be used included.
func Drain(ctx context.Context, args []string, stderr io.Writer, sinkFor func() (Sink, error)) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "usage: livy drain")
		return 2
	}
	err := drain(ctx, hook.Settings(), stderr, sinkFor)
	switch {
	case errors.Is(err, ErrNoDrainSource):
		fmt.Fprintf(stderr, "drain: %v\n", err)
		return 2
	case err != nil:
		drainReport(stderr, err.Error())
		return 1
	}
	return 0
}

// DailyDrain is the daily job's drain: nothing at all when no drain.source is set, and a failure
// is said and recorded but never stops the upload that follows.
func DailyDrain(ctx context.Context, stderr io.Writer, sinkFor func() (Sink, error)) {
	if err := drain(ctx, hook.Settings(), stderr, sinkFor); err != nil && !errors.Is(err, ErrNoDrainSource) {
		drainReport(stderr, err.Error())
	}
}

func drain(ctx context.Context, res config.Resolved, stderr io.Writer, sinkFor func() (Sink, error)) error {
	src, at, err := drainSource(res)
	if err != nil {
		return err
	}
	sink, err := sinkFor()
	if err != nil {
		return fmt.Errorf("%w; nothing drained", err)
	}
	dir := hook.DataDir()
	// emit's lock: a git sink's clone must not see emit's push and drain's at once.
	release, err := ledger.Lock(filepath.Join(dir, LockName), emitLockWait)
	if err != nil {
		return fmt.Errorf("lock %s: %w", LockName, err)
	}
	defer release()
	d := &drainer{src: src, sink: sink, stderr: stderr, statePath: filepath.Join(dir, drainStateName)}
	n, err := d.run(ctx, at)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "drain: %d copied, %d already there, %d refused, from %s\n", n.copied, n.present, n.refused, at.remote)
	return nil
}

// drainReport says a failure on stderr and as a ledger.error, which ships with the next upload.
func drainReport(stderr io.Writer, reason string) {
	fmt.Fprintf(stderr, "drain: %s\n", reason)
	hook.WriteError("drain: "+reason, stderr)
}

// drainRepo is the repository drained: a git sink's remote, read through its own clone.
type drainRepo interface {
	Fetch(ctx context.Context) (tip string, err error)
	Added(ctx context.Context, from, to string) ([]string, error)
	Read(ctx context.Context, commit, path string, limit int64) ([]byte, error)
	URL(key string) string
}

// drainPlace is the remote and branch a drain state belongs to.
type drainPlace struct{ remote, branch string }

// drainer is one pass from src into sink.
type drainer struct {
	src       drainRepo
	sink      Sink
	stderr    io.Writer
	statePath string
}

func (d *drainer) run(ctx context.Context, at drainPlace) (drainCount, error) {
	var n drainCount
	tip, err := d.src.Fetch(ctx)
	if err != nil || tip == "" {
		return n, err
	}
	st := d.load(at)
	if st.Commit == tip && len(st.Refused) == 0 {
		return n, nil
	}
	paths, err := d.candidates(ctx, st, tip)
	if err != nil {
		return n, err
	}
	refused := map[string]string{}
	for _, p := range paths {
		reason, err := d.one(ctx, tip, p, &n)
		if err != nil {
			return n, err // nothing recorded: the next run starts from the same commit
		}
		if reason != "" {
			refused[p] = reason
		}
	}
	d.reportRefused(st.Refused, refused)
	st.Commit, st.Refused = tip, refused
	n.refused = len(refused)
	return n, d.save(st)
}

// candidates is every object to look at: the files added since the last drained commit, and the
// ones refused before that are still there.
func (d *drainer) candidates(ctx context.Context, st drainState, tip string) ([]string, error) {
	added, err := d.src.Added(ctx, st.Commit, tip)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, p := range added {
		set[p] = true
	}
	if len(st.Refused) > 0 {
		all, err := d.src.Added(ctx, "", tip)
		if err != nil {
			return nil, err
		}
		for _, p := range all {
			if _, was := st.Refused[p]; was {
				set[p] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(set)), nil
}

// one drains one file. A file outside source= (a README) is not an object and is skipped. The
// reason is why the object is refused; the error is a failure that stops the pass.
func (d *drainer) one(ctx context.Context, tip, path string, n *drainCount) (reason string, err error) {
	if !strings.HasPrefix(path, "source=") {
		return "", nil
	}
	gz, err := d.src.Read(ctx, tip, path, maxDrainObject)
	if ctx.Err() != nil {
		return "", fmt.Errorf("stopped: %w", ctx.Err())
	}
	if err != nil {
		return err.Error(), nil
	}
	raw, err := checkDrained(path, gz)
	if err != nil {
		return err.Error(), nil
	}
	body, err := Gzip(raw)
	if err != nil {
		return "", fmt.Errorf("gzip %s: %w", path, err)
	}
	putCtx, cancel := context.WithTimeout(ctx, putTimeout)
	err = d.sink.Put(putCtx, path, body)
	cancel()
	switch {
	case errors.Is(err, fs.ErrExist):
		n.present++
	case err != nil:
		return "", fmt.Errorf("%s not copied, the pass stops here and the next run retries it: %w", path, err)
	default:
		n.copied++
	}
	return "", nil
}

// checkDrained is the object's segment bytes, or why the object is not one emit could have
// written: its key is not an emit key, or not the one emit gives these bytes, or a line is not
// what its stream holds.
func checkDrained(key string, gz []byte) ([]byte, error) {
	m := drainKey.FindStringSubmatch(key)
	if m == nil {
		return nil, errors.New("not an object key livy emit writes")
	}
	stream, day, handle := m[1], m[2], m[3]
	raw, err := inflate(gz)
	if err != nil {
		return nil, err
	}
	if err := checkLines(stream, raw); err != nil {
		return nil, err
	}
	rotated, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return nil, fmt.Errorf("dt=%s is not a day", day)
	}
	want, _, err := keyFor(stream, raw, rotated, handle)
	if err != nil {
		return nil, err
	}
	if want != key {
		return nil, fmt.Errorf("its bytes are the object %s", want)
	}
	return raw, nil
}

func inflate(gz []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("not gzip: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, maxDrainInflate+1))
	if err != nil {
		return nil, fmt.Errorf("not gzip: %w", err)
	}
	if len(raw) > maxDrainInflate {
		return nil, fmt.Errorf("inflates past %d bytes", maxDrainInflate)
	}
	return raw, nil
}

// checkLines refuses a segment with a line its stream does not hold: a ledger line must be an
// event the schema allows; an otel line must be a JSON object. Another stream is refused.
func checkLines(stream string, raw []byte) error {
	check := func(line []byte) error { _, err := event.Decode(line); return err }
	switch stream {
	case source:
	case OtelSource:
		check = func(line []byte) error {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(line, &obj); err != nil {
				return fmt.Errorf("not a JSON object: %w", err)
			}
			if obj == nil {
				return errors.New("null, not a JSON object")
			}
			return nil
		}
	default:
		return fmt.Errorf("%s is not a stream livy emit writes", stream)
	}
	lines := 0
	for i, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		lines++
		if err := check(line); err != nil {
			return fmt.Errorf("line %d: %w", i+1, err)
		}
	}
	if lines == 0 {
		return errors.New("no lines")
	}
	return nil
}

// reportRefused writes one ledger.error for the objects refused this run that were not refused
// for the same reason before.
func (d *drainer) reportRefused(before, now map[string]string) {
	var fresh []string
	for _, k := range slices.Sorted(maps.Keys(now)) {
		if before[k] != now[k] {
			fresh = append(fresh, fmt.Sprintf("%s (%s)", d.src.URL(k), now[k]))
		}
	}
	if len(fresh) == 0 {
		return
	}
	more := ""
	if len(fresh) > maxReported {
		more = fmt.Sprintf(" and %d more", len(fresh)-maxReported)
		fresh = fresh[:maxReported]
	}
	drainReport(d.stderr, fmt.Sprintf("refused %s%s; not copied, tried again each run", strings.Join(fresh, "; "), more))
}

// load is the state for at's remote and branch; another remote or branch starts from nothing.
func (d *drainer) load(at drainPlace) drainState {
	fresh := drainState{Remote: at.remote, Branch: at.branch}
	raw, err := os.ReadFile(d.statePath)
	if errors.Is(err, fs.ErrNotExist) {
		return fresh
	}
	var st drainState
	if err == nil {
		err = json.Unmarshal(raw, &st)
	}
	if err != nil {
		fmt.Fprintf(d.stderr, "drain: %s unreadable (%v); reading the whole repository again\n", drainStateName, err)
		return fresh
	}
	if st.Remote != at.remote || st.Branch != at.branch {
		return fresh
	}
	return st
}

func (d *drainer) save(st drainState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encode %s: %w", drainStateName, err)
	}
	tmp := d.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", drainStateName, err)
	}
	if err := os.Rename(tmp, d.statePath); err != nil {
		return fmt.Errorf("write %s: %w", drainStateName, err)
	}
	return nil
}
