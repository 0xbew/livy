// Package emit is `livy emit`: rotate the local ledger, validate each segment, gzip it,
// and ship it to the configured sink. It runs outside the tool loop (detached at session
// end, throttled mid-session, and daily from the scheduler) and is the only part of
// recording that opens a connection.
//
// The rules it keeps, each from the outcome contract:
//   - An object's key is the hash of the segment's bytes, its day comes from the segment
//     and its handle is frozen when the segment is rotated, so a retry targets the same key.
//   - Write-once: a sink never replaces an object, and "already there" counts as shipped.
//     A segment is deleted only after the sink confirms it.
//   - A refused pass writes one ledger.error, keeps everything, and does not rotate again
//     until the bucket takes the backlog (except across a UTC day), so a refusing bucket
//     does not grow it.
//   - Never blocks: every path exits 0. Never silent: every failure is a ledger.error.
package emit

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/extract/cli"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/ledger"
)

// Sink is where segments go. Put creates the object at key and never replaces one: an
// object already at key returns an error wrapping fs.ErrExist, which means the sink holds
// these exact bytes (the key is their hash). URL names an object for people.
type Sink interface {
	Put(ctx context.Context, key string, body []byte) error
	URL(key string) string
}

// Names inside the data folder. They match the Python tool's, so a folder it left behind
// ships under the same keys.
const (
	SegmentsDir = ledger.SegmentsDir
	// UploadedDir keeps a copy of each ledger segment the sink has, so this machine can still
	// read what it recorded (livy why) after the upload.
	UploadedDir   = ledger.UploadedDir
	RejectedDir   = "rejected"
	LockName      = "emit.lock"
	LastRunName   = "emit-last-run"
	LastUploadKey = "last-upload.json"
	anonymous     = "_no-handle"
	source        = "source=agent-hooks"
)

// DataNames are the files and folders this package keeps in the data folder, by exact name.
// `livy uninstall --purge` deletes these and nothing else.
func DataNames() []string {
	return []string{SegmentsDir, UploadedDir, RejectedDir, LockName, LastRunName, LastUploadKey, LastTextUpload, gitCloneDir}
}

// Timing. A throttled run (mid-session) never waits for another emitter: that one has the
// pass in hand. Session end and the daily flush wait, so a session's last line ships now.
var (
	emitLockWait   = 60 * time.Second
	rotateLockWait = 10 * time.Second
	putTimeout     = 120 * time.Second
	retryAfter     = 120 * time.Second
	staleTemp      = time.Minute
)

// LastUpload is the record doctor reads to tell a quiet machine from a broken one.
type LastUpload struct {
	TS     string `json:"ts"`
	Object string `json:"object"`
}

// Options are one run's choices.
type Options struct {
	DryRun   bool
	Throttle time.Duration // 0: no throttle
	Event    string        // written (payload on stdin) before the sweep
	Extract  bool          // run a prompt pass, then a transcript extraction pass inside the emit lock, first
}

// Run is `livy emit [args]`; it always returns 0. sinkFor resolves the configured sink:
// nil and an error when none is usable, which is said and leaves everything in place.
func Run(ctx context.Context, args []string, stdin *os.File, stderr io.Writer, sinkFor func() (Sink, error)) (code int) {
	defer func() {
		if p := recover(); p != nil {
			code = 0
			report(stderr, fmt.Sprintf("unhandled panic: %v", p))
		}
	}()
	opts, err := parseArgs(args)
	if err != nil {
		report(stderr, err.Error())
		return 0
	}
	// A dry run says what it would do and writes nothing: no event, no ledger.error, no marker.
	if opts.DryRun {
		stderr = dryWriter{stderr}
	}
	if opts.Event != "" && opts.DryRun {
		fmt.Fprintf(stderr, "emit: dry-run, would write %s first\n", opts.Event)
	} else if opts.Event != "" {
		hook.Run([]string{opts.Event}, stdin, stderr)
	}
	dir := hook.DataDir()
	if opts.Throttle > 0 {
		if st, err := os.Stat(filepath.Join(dir, LastRunName)); err == nil && clock.Now().Sub(st.ModTime()) < opts.Throttle {
			return 0 // quiet on purpose: this runs after every prompt, turn and tool call
		}
	}
	if opts.Extract && !opts.DryRun {
		// Before the emit lock (it may read two weeks of transcripts), so what it captures
		// ships in this sweep.
		cli.PromptPass(stderr)
	}
	sink, err := sinkFor()
	if err != nil {
		keepLocal(opts, dir, err, stderr)
		return 0
	}
	if opts.DryRun {
		drySweep(ctx, opts, dir, sink, stderr)
		return 0
	}
	shipLocked(ctx, opts, dir, sink, stderr)
	return 0
}

// keepLocal is a run with no usable sink: extraction still runs, the reason is said, and
// the ledger stays where it is.
func keepLocal(opts Options, dir string, err error, stderr io.Writer) {
	if opts.Extract && !opts.DryRun {
		extractPass(opts, stderr) // usage is captured even while nothing can ship
	}
	if errors.Is(err, ErrNoSink) {
		// A machine set up to keep its data local: doctor says so, the ledger does not.
		fmt.Fprintf(stderr, "emit: %v; nothing uploaded, the ledger is kept\n", err)
	} else {
		report(stderr, err.Error()+"; nothing uploaded, the ledger is kept")
	}
	if !opts.DryRun {
		touch(dir, 0, stderr)
	}
}

// drySweep says what a run would ship, and writes nothing.
func drySweep(ctx context.Context, opts Options, dir string, sink Sink, stderr io.Writer) {
	if opts.Extract {
		// Not run: extraction takes its own lock file, and a dry run creates nothing.
		fmt.Fprintln(stderr, "emit: dry-run, would run an extraction pass first")
	}
	// No lock: taking it would create the data folder and emit.lock. A dry run only reads.
	for _, p := range []*pass{{dir: dir, sink: sink, stderr: stderr, dry: true}, otelPass(dir, sink, stderr, true)} {
		if p == nil {
			continue
		}
		p.sweep(ctx)
		for _, msg := range p.problems {
			fmt.Fprintf(stderr, "emit: dry-run: %s\n", msg)
		}
	}
}

// shipLocked takes the emit lock and ships the ledger, then the otel stream, then prompt text.
func shipLocked(ctx context.Context, opts Options, dir string, sink Sink, stderr io.Writer) {
	wait := emitLockWait
	if opts.Throttle > 0 {
		wait = 0
	}
	release, err := ledger.Lock(filepath.Join(dir, LockName), wait)
	if errors.Is(err, ledger.ErrHeld) {
		if opts.Throttle == 0 {
			fmt.Fprintln(stderr, "emit: another emitter held the lock; leaving the sweep to it")
		}
		return
	}
	if err != nil {
		report(stderr, fmt.Sprintf("cannot open %s (%v); nothing uploaded, the ledger is kept", LockName, err))
		return
	}
	defer release()
	if opts.Extract {
		// Inside the emit lock and before rotation, so what this pass extracts ships in it:
		// at session end, the session's usage goes with its session.ended.
		extractPass(opts, stderr)
	}
	p := &pass{dir: dir, sink: sink, stderr: stderr, dry: opts.DryRun}
	keepFor, problem := keepUploaded()
	if problem != "" {
		p.say("%s", problem)
	}
	p.keepFor = keepFor
	if !opts.DryRun {
		p.prune()
	}
	p.sweep(ctx)
	if len(p.problems) > 0 && ctx.Err() == nil { // a stop is not a failure to record
		report(stderr, strings.Join(p.problems, "; "))
	}
	// The otel stream, after the ledger: it has its own segments, lock and upload record. Its
	// shipped segments are kept as long as the ledger's, for `livy profile`.
	if op := otelPass(dir, sink, stderr, opts.DryRun); op != nil {
		op.keepFor = keepFor
		if !opts.DryRun {
			op.prune()
		}
		op.sweep(ctx)
		if len(op.problems) > 0 && ctx.Err() == nil {
			report(stderr, "otel stream: "+strings.Join(op.problems, "; "))
		}
	}
	// The prompt text stream, last: it never holds the ledger's upload back.
	promptPass(ctx, dir, stderr, opts.DryRun)
	if !opts.DryRun {
		backdate := time.Duration(0)
		if p.waiting() {
			backdate = max(0, opts.Throttle-retryAfter) // retry sooner, but not on every trigger
		}
		touch(dir, backdate, stderr)
	}
}

// otelPass is a pass over the otel stream (<data dir>/otel, written by the receiver), or nil
// when there is none. The stream is laid out as the ledger is (ledger.jsonl, segments/,
// spill/, rejected/, last-upload.json), so the same pass ships it, under OtelSource.
func otelPass(dir string, sink Sink, stderr io.Writer, dry bool) *pass {
	od := filepath.Join(dir, "otel")
	if st, err := os.Stat(od); err != nil || !st.IsDir() {
		return nil
	}
	return &pass{dir: od, source: OtelSource, sink: sink, stderr: stderr, dry: dry}
}

func parseArgs(args []string) (Options, error) {
	var o Options
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		next := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("bad arguments: %s needs a value", name)
			}
			i++
			return args[i], nil
		}
		switch name {
		case "--dry-run":
			o.DryRun = true
		case "--throttle":
			v, err := next()
			if err != nil {
				return o, err
			}
			s, perr := strconv.ParseFloat(v, 64)
			if perr != nil || s < 0 || math.IsNaN(s) || math.IsInf(s, 0) || s > 366*86400 {
				return o, fmt.Errorf("bad arguments: --throttle %q is not a number of seconds", v)
			}
			o.Throttle = time.Duration(s * float64(time.Second))
		case "--extract":
			o.Extract = true
		case "--event":
			v, err := next()
			if err != nil {
				return o, err
			}
			o.Event = v
		default:
			return o, fmt.Errorf("bad arguments: unknown argument %q", a)
		}
	}
	return o, nil
}

// extractPass runs one `livy extract` pass (every transcript reader), with the same
// throttle as this emit. Its counts are not printed: an async hook's output
// would be attached to the session.
func extractPass(opts Options, stderr io.Writer) {
	args := []string{"--no-prompt-pass"} // never inside the emit lock: it reads two weeks of transcripts
	if opts.Throttle > 0 {
		args = append(args, "--throttle", strconv.FormatFloat(opts.Throttle.Seconds(), 'f', -1, 64))
	}
	cli.Main(args, io.Discard, stderr)
}

// report says a failure on stderr and as a ledger.error: at session end stderr reaches
// nobody, and the error line ships with the next segment. A dry run only says it.
func report(stderr io.Writer, reason string) {
	fmt.Fprintf(stderr, "emit: %s\n", reason)
	if _, dry := stderr.(dryWriter); !dry {
		hook.WriteError("emit: "+reason, stderr)
	}
}

// dryWriter marks a dry run's stderr, so report writes no ledger.error.
type dryWriter struct{ io.Writer }

// touch marks a run, backdated so a run that left work waiting retries sooner. stderr
// only: a ledger.error here would itself be the next thing to upload.
func touch(dir string, backdate time.Duration, stderr io.Writer) {
	path := filepath.Join(dir, LastRunName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err == nil {
		f.Close()
		t := clock.Now().Add(-backdate)
		err = os.Chtimes(path, t, t)
	}
	if err != nil {
		fmt.Fprintf(stderr, "emit: could not update %s: %v\n", LastRunName, err)
	}
}

// pass is one sweep under the emit lock.
type pass struct {
	dir      string
	source   string // the object prefix: "" is source=agent-hooks
	sink     Sink
	stderr   io.Writer
	dry      bool
	problems []string
	blocked  bool
	keepFor  time.Duration // how long shipped segments are kept; 0 deletes them
}

func (p *pass) say(format string, a ...any) {
	p.problems = append(p.problems, fmt.Sprintf(format, a...))
}

func (p *pass) sweep(ctx context.Context) {
	if !p.dry {
		p.clearTemps()
		p.foldSpills()
	}
	for _, seg := range p.pending() {
		if !p.ship(ctx, seg) {
			p.blocked = true
			break
		}
	}
	ledgerPath := filepath.Join(p.dir, ledger.FileName)
	if p.dry {
		if raw, err := os.ReadFile(ledgerPath); err == nil && len(raw) > 0 {
			key, _, _ := keyFor(p.source, raw, clock.Now(), Handle())
			fmt.Fprintf(p.stderr, "emit: dry-run, would upload %s\n", p.sink.URL(key))
		}
		return
	}
	if p.blocked {
		// Nothing can ship this pass. Keep the live ledger as one growing file rather than
		// a new segment per pass, except across a UTC day, so an outage still leaves one
		// segment per day under the day its events happened.
		if day := firstEventDay(ledgerPath); day != "" && day != clock.Now().Format("2006-01-02") {
			p.rotate()
		}
		return
	}
	if seg := p.rotate(); seg != "" {
		p.ship(ctx, seg)
	}
}

// rotate moves the live ledger to a new segment under the ledger lock, so no hook can
// still be writing into it once it is read. The segment's folder is the handle now.
func (p *pass) rotate() string {
	ledgerPath := filepath.Join(p.dir, ledger.FileName)
	owned := filepath.Join(p.dir, SegmentsDir, handleDir(Handle()))
	var target string
	var rotated bool
	acquired, err := ledger.WithLock(p.dir, rotateLockWait, func() error {
		st, err := os.Stat(ledgerPath)
		if err != nil || st.Size() == 0 {
			return nil
		}
		if err := os.MkdirAll(owned, 0o700); err != nil {
			return err
		}
		target = freeName(filepath.Join(owned, segmentName(clock.Now(), "")))
		if err := os.Rename(ledgerPath, target); err != nil {
			return err
		}
		rotated = true
		return nil
	})
	switch {
	case !acquired && err != nil:
		p.say("ledger lock not acquired (%v); not rotating, the next run will", err)
	case !acquired:
		p.say("ledger lock not acquired; not rotating, the next run will")
	case err != nil:
		p.say("could not rotate the ledger: %v", err)
	}
	if !rotated {
		return ""
	}
	return target
}

// foldSpills turns each line file a hook spilled (it could not get the ledger lock in
// time) into a segment of its own. A spill file is complete once it has its final name.
func (p *pass) foldSpills() {
	spillDir := filepath.Join(p.dir, ledger.SpillDir)
	names, err := filepath.Glob(filepath.Join(spillDir, "*.jsonl"))
	if err != nil || len(names) == 0 {
		return
	}
	sort.Strings(names)
	owned := filepath.Join(p.dir, SegmentsDir, handleDir(Handle()))
	if err := os.MkdirAll(owned, 0o700); err != nil {
		p.say("could not fold spilled lines: %v", err)
		return
	}
	now := clock.Now()
	for i, name := range names {
		target := freeName(filepath.Join(owned, segmentName(now, "s"+strconv.Itoa(i))))
		if err := os.Rename(name, target); err != nil {
			p.say("could not fold %s: %v", filepath.Base(name), err)
		}
	}
}

// clearTemps removes temp files a killed writer left: a spill temp older than a minute
// (a newer one may belong to a hook still writing), and this package's own fixed temps.
func (p *pass) clearTemps() {
	temps, _ := filepath.Glob(filepath.Join(p.dir, ledger.SpillDir, ".*.tmp"))
	for _, t := range temps {
		if st, err := os.Stat(t); err == nil && clock.Now().Sub(st.ModTime()) > staleTemp {
			_ = os.Remove(t)
		}
	}
	_ = os.Remove(filepath.Join(p.dir, LastUploadKey+".tmp"))
}

// pending is every segment waiting to ship, oldest first.
func (p *pass) pending() []string {
	var out []string
	root := filepath.Join(p.dir, SegmentsDir)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
			out = append(out, path)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return filepath.Base(out[i]) < filepath.Base(out[j]) })
	return out
}

func (p *pass) waiting() bool { return len(p.pending()) > 0 }

// ship validates one segment, uploads it, and deletes it once the sink has it. False when
// it is still waiting.
func (p *pass) ship(ctx context.Context, seg string) bool {
	raw, err := os.ReadFile(seg)
	if err != nil {
		p.say("cannot read %s: %v", filepath.Base(seg), err)
		return false
	}
	good, bad := countLines(raw)
	if good == 0 && p.dry {
		fmt.Fprintf(p.stderr, "emit: dry-run, would refuse %s: no parseable JSON lines\n", filepath.Base(seg))
		return true
	}
	if good == 0 {
		// Never retried: it can never succeed, and the retry noise would train people to
		// ignore the error.
		where := "moved to rejected/"
		err := os.MkdirAll(filepath.Join(p.dir, RejectedDir), 0o700)
		if err == nil {
			err = os.Rename(seg, freeName(filepath.Join(p.dir, RejectedDir, filepath.Base(seg))))
		}
		if err != nil {
			where = fmt.Sprintf("could not move it to rejected/ (%v); it will be refused again", err)
		}
		p.say("refusing %s: no parseable JSON lines (%d unparseable); %s, not uploaded", filepath.Base(seg), bad, where)
		return true // handled: it must not hold back the segments behind it
	}
	if bad > 0 {
		p.say("%s has %d unparseable line(s); shipping all its bytes", filepath.Base(seg), bad)
	}
	key, dayFromTS, err := keyFor(p.source, raw, stampTime(seg), handleOf(p.dir, seg))
	if err != nil {
		p.say("%s: %v", filepath.Base(seg), err)
		return false
	}
	if !dayFromTS {
		p.say("%s has no timezone-aware ts; its day is the day it was rotated", filepath.Base(seg))
	}
	if p.dry {
		fmt.Fprintf(p.stderr, "emit: dry-run, would upload %s\n", p.sink.URL(key))
		return true
	}
	body, err := Gzip(raw)
	if err != nil {
		p.say("could not gzip %s: %v", filepath.Base(seg), err)
		return false
	}
	putCtx, cancel := context.WithTimeout(ctx, putTimeout)
	err = p.sink.Put(putCtx, key, body)
	cancel()
	switch {
	case errors.Is(err, fs.ErrExist):
		fmt.Fprintf(p.stderr, "emit: already there, counted as shipped: %s\n", p.sink.URL(key))
	case err != nil:
		p.say("%s not uploaded, kept for the next run: %v", filepath.Base(seg), err)
		return false
	default:
		fmt.Fprintf(p.stderr, "emit: shipped %s\n", p.sink.URL(key))
	}
	p.recordUpload(key)
	p.keep(seg)
	return true
}

// recordUpload writes last-upload.json through one fixed temp name. stderr only: a
// ledger.error here would become the next thing to upload.
func (p *pass) recordUpload(key string) {
	path := filepath.Join(p.dir, LastUploadKey)
	b, _ := json.Marshal(LastUpload{TS: clock.Seconds(clock.Now()), Object: p.sink.URL(key)})
	tmp := path + ".tmp"
	err := os.WriteFile(tmp, b, 0o600)
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		fmt.Fprintf(p.stderr, "emit: shipped, but could not record the last upload: %v\n", err)
	}
}

// Key is the object key for a segment's bytes: source=agent-hooks/dt=<day>/handle=<h>/
// <first 16 hex of sha256(raw)>.jsonl.gz. The day is the UTC day of the first line with a
// timezone-aware ts; with none, the day the segment was rotated, never the upload clock.
// With no handle the handle= part is left out.
func Key(raw []byte, rotated time.Time, handle string) (key string, dayFromTS bool, err error) {
	return keyFor(source, raw, rotated, handle)
}

// OtelSource is the prefix of the otel stream's objects: the OTLP exports Livy's receiver
// stored (relay sink mode), named by the same rule as the ledger's.
const OtelSource = "source=otel"

func keyFor(src string, raw []byte, rotated time.Time, handle string) (key string, dayFromTS bool, err error) {
	if src == "" {
		src = source
	}
	day := ""
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if d := utcDay(rec["ts"]); d != "" {
			day, dayFromTS = d, true
			break
		}
	}
	if day == "" {
		if rotated.IsZero() {
			return "", false, errors.New("no timezone-aware ts and no rotation time; cannot name its day")
		}
		day = rotated.UTC().Format("2006-01-02")
	}
	sum := sha256.Sum256(raw)
	prefix := src + "/dt=" + day
	if handle != "" {
		prefix += "/handle=" + handle
	}
	return prefix + "/" + hex.EncodeToString(sum[:])[:16] + ".jsonl.gz", dayFromTS, nil
}

// utcDay is the UTC date of a timezone-aware ISO 8601 timestamp, else "". A naive one is
// refused: its instant is not known.
func utcDay(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	s = strings.TrimSpace(s)
	// Python's fromisoformat also takes a space for the T and an offset without a colon.
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05.999999999-0700", "2006-01-02 15:04:05.999999999-0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format("2006-01-02")
		}
	}
	return ""
}

// Gzip compresses deterministically: no name and a zero mtime, so the same segment always
// gives the same object.
func Gzip(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func countLines(raw []byte) (good, bad int) {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec map[string]any
		if json.Unmarshal(line, &rec) == nil {
			good++
		} else {
			bad++
		}
	}
	return good, bad
}

func firstEventDay(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	n, _ := io.ReadFull(f, buf)
	line, _, _ := bytes.Cut(buf[:n], []byte("\n"))
	var rec map[string]any
	if json.Unmarshal(line, &rec) != nil {
		return ""
	}
	return utcDay(rec["ts"])
}

// Handle is the configured handle, cut to what an object key may hold: letters, digits
// and hyphens, lowercased. "" when none is set.
func Handle() string {
	s, _ := hook.Settings().Settings["identity.handle"].Value.(string)
	return CleanHandle(s)
}

// CleanHandle is a handle as an object key holds it: letters, digits and hyphens, lowercased.
func CleanHandle(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if r == '-' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') {
			b.WriteRune(r)
		}
	}
	return strings.ToLower(b.String())
}

func handleDir(h string) string {
	if h == "" {
		return anonymous
	}
	return h
}

// handleOf is the handle frozen with a segment: its folder under segments/. A segment
// directly in segments/ (an old layout) or under _no-handle has none.
func handleOf(dir, seg string) string {
	parent := filepath.Dir(seg)
	if parent == filepath.Join(dir, SegmentsDir) || filepath.Base(parent) == anonymous {
		return ""
	}
	return filepath.Base(parent)
}

// segmentName is <UTC stamp, microseconds>-<pid>[-tag].jsonl; the stamp is the rotation
// time, which names the segment's day when no line has an aware ts.
func segmentName(t time.Time, tag string) string {
	name := t.UTC().Format("20060102T150405.000000Z")
	name = strings.Replace(name, ".", "", 1) + "-" + strconv.Itoa(os.Getpid())
	if tag != "" {
		name += "-" + tag
	}
	return name + ".jsonl"
}

// freeName is path, or path with a counter before .jsonl when that name is taken, so a
// rename never replaces a segment (a clock that stepped back, or a pinned one).
func freeName(path string) string {
	base := strings.TrimSuffix(path, ".jsonl")
	for i := 1; ; i++ {
		// Taken means Lstat found it; any other answer (no permission on the folder) is
		// returned as is, and the rename that follows fails and says why.
		if _, err := os.Lstat(path); err != nil {
			return path
		}
		path = base + "-" + strconv.Itoa(i) + ".jsonl"
	}
}

func stampTime(seg string) time.Time {
	base := filepath.Base(seg)
	if len(base) < 15 {
		return time.Time{}
	}
	t, err := time.Parse("20060102T150405", base[:15])
	if err != nil {
		if st, serr := os.Stat(seg); serr == nil {
			return st.ModTime()
		}
		return time.Time{}
	}
	return t
}
