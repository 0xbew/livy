package extract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/event"
	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/ledgerjson"
	"github.com/0xbew/livy/internal/plainname"
	"github.com/0xbew/livy/internal/pytext"
)

// SchemaVersion is the ledger line contract the extractors write under.
const SchemaVersion = 1

// MaxNameChars bounds a NAME (tool, skill, error kind, stop reason). A value past it, or one
// that could carry a path or a sentence (a slash, a backslash, whitespace), is not a name.
const MaxNameChars = plainname.MaxChars

// IsName is true for a plain name: 1..200 characters, none of them whitespace or a slash.
func IsName(s string) bool { return plainname.Valid(s) }

// Int is a JSON value as a non-negative integer, the way the Python reader accepts one: an
// integer literal (not a float, not a bool).
func Int(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok || strings.ContainsAny(string(n), ".eE") {
		return 0, false
	}
	i, err := strconv.ParseInt(string(n), 10, 64)
	if err != nil || i < 0 {
		return 0, false
	}
	return i, true
}

// Str is v when it is a non-empty string.
func Str(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok && s != ""
}

// PyStr is Python's str() of a decoded JSON value, for the seeds of fallback ids.
func PyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case json.Number:
		return string(t)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// Decode parses one JSON line as Python's json.loads would for the fields read here.
func Decode(raw []byte) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("data after the value")
	}
	return v, nil
}

// ReadRecords reads the file at path whole and calls fn with each complete line that is a
// JSON object, and the byte offset the line starts at. It returns the offset after the last
// complete line and the count of lines that were not JSON objects. A last line with no
// newline yet is left unread.
func ReadRecords(path string, fn func(rec map[string]any, at int64)) (offset int64, bad int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("read records: %w", err)
	}
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		raw := data[:i+1]
		data = data[i+1:]
		at := offset
		offset += int64(len(raw))
		v, err := Decode([]byte(pytext.DecodeReplace(raw)))
		rec, ok := v.(map[string]any)
		if err != nil || !ok {
			bad++
			continue
		}
		fn(rec, at)
	}
	return offset, bad, nil
}

var isoRe = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2}).(\d{2})(?::(\d{2})(?::(\d{2})(?:\.(\d+))?)?)?` +
	`(?:([+-])(\d{2}):(\d{2})(?::(\d{2})(?:\.(\d{1,6}))?)?)?$`)

// UTCInstant parses an ISO 8601 timestamp that carries its zone ("Z" or an offset). A time
// with no zone is not an instant, and nil.
func UTCInstant(ts any) *time.Time {
	s, ok := ts.(string)
	if !ok {
		return nil
	}
	s = pytext.Strip(s)
	if s == "" {
		return nil
	}
	if strings.HasSuffix(s, "Z") || strings.HasSuffix(s, "z") {
		s = s[:len(s)-1] + "+00:00"
	}
	m := isoRe.FindStringSubmatch(s)
	if m == nil || m[8] == "" {
		return nil
	}
	num := func(x string) int {
		n, _ := strconv.Atoi(x)
		return n
	}
	frac := 0
	if m[7] != "" {
		digits := (m[7] + "000000")[:6]
		frac = num(digits)
	}
	y, mo, d, h := num(m[1]), num(m[2]), num(m[3]), num(m[4])
	mi, sec := num(m[5]), num(m[6])
	if mo < 1 || mo > 12 || d < 1 || d > daysIn(y, mo) || h > 23 || mi > 59 || sec > 59 || y < 1 {
		return nil
	}
	offH, offM, offS := num(m[9]), num(m[10]), num(m[11])
	if offH > 23 || offM > 59 || offS > 59 {
		return nil
	}
	off := offH*3600 + offM*60 + offS
	if m[8] == "-" {
		off = -off
	}
	t := time.Date(y, time.Month(mo), d, h, mi, sec, frac*1000, time.FixedZone("", off)).UTC()
	if t.Year() < 1 || t.Year() > 9999 {
		return nil
	}
	return &t
}

func daysIn(y, m int) int {
	return time.Date(y, time.Month(m)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// UTCSeconds is the instant as isoformat(timespec="seconds") in UTC, or "" when unparseable.
func UTCSeconds(ts any) string {
	t := UTCInstant(ts)
	if t == nil {
		return ""
	}
	return clock.Seconds(*t)
}

// Writer appends validated lines to the ledger in batches and writes this run's errors.
type Writer struct {
	Dir     string
	Stderr  io.Writer
	DryRun  bool
	Written int
	Invalid int
	Failed  int
}

// Write appends lines; false when any valid line failed to reach the ledger.
func (w *Writer) Write(lines []map[string]any) bool {
	if w.DryRun || len(lines) == 0 {
		return true
	}
	var buf []byte
	n := 0
	for _, rec := range lines {
		line, ok := encode(rec)
		if !ok {
			w.Invalid++
			continue
		}
		buf = append(append(buf, line...), '\n')
		n++
	}
	if n == 0 {
		return true
	}
	done, err := ledger.Append(w.Dir, buf)
	if err != nil && !errors.Is(err, ledger.ErrClose) {
		w.Failed += n
		return false
	}
	w.Written += n
	if done.Unchecked != nil {
		w.Error(fmt.Sprintf("ledger: the last line could not be checked for a torn write (%v); %d lines written without it", done.Unchecked, n))
	}
	return true
}

// encode validates one line against the schema and encodes it as the Python ledger does.
func encode(rec map[string]any) ([]byte, bool) {
	for _, k := range []string{"ts", "event", "repo"} {
		if s, _ := rec[k].(string); s == "" {
			return nil, false
		}
	}
	raw, err := ledgerjson.Marshal(rec)
	if err != nil {
		return nil, false
	}
	e, err := event.Decode(raw)
	if err != nil {
		return nil, false
	}
	line, err := e.Encode()
	return line, err == nil
}

// Error writes one ledger.error. Extraction is machine-level work, so it names no repo.
func (w *Writer) Error(reason string) {
	fmt.Fprintln(w.Stderr, reason)
	if w.DryRun {
		return
	}
	rec := map[string]any{"schema_version": int64(SchemaVersion), "ts": clock.Seconds(clock.Now()),
		"event": "ledger.error", "repo": Unknown, "reason": reason}
	line, ok := encode(rec)
	if !ok {
		return
	}
	if _, err := ledger.Append(w.Dir, append(line, '\n')); err != nil && !errors.Is(err, ledger.ErrClose) {
		fmt.Fprintf(w.Stderr, "ledger: could not write a ledger.error (%s)\n", reason)
	}
}

// SaveJSON replaces path atomically through one fixed temporary name, which a killed save
// leaves behind for the next save to overwrite.
func SaveJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Offset is a stored offset, if it is one: a non-negative integer.
func Offset(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		return Int(t)
	case float64:
		if t >= 0 && t == float64(int64(t)) {
			return int64(t), true
		}
	case int64:
		return t, t >= 0
	}
	return 0, false
}

// SubMap is root[key] as a map, replaced with an empty one when it is anything else.
func SubMap(root map[string]any, key string) map[string]any {
	m, ok := root[key].(map[string]any)
	if !ok {
		m = map[string]any{}
		root[key] = m
	}
	return m
}

// Touch sets path's mtime to now minus age, creating it.
func Touch(path string, age time.Duration) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	when := clock.Now().Add(-age)
	return os.Chtimes(path, when, when)
}
