package relay

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/extract/codex"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/otelignore"
	"github.com/0xbew/livy/internal/otelredact"
)

// Sink mode: the relay is Livy's own OTLP receiver. Each admitted export is decoded as
// OTLP/JSON, redacted by the collector's rules (otelredact), and appended as one line to
// <data dir>/otel/ledger.jsonl, a stream of its own beside the hook ledger, which emit ships
// through the configured sink under source=otel/. The export is answered once its line is on
// disk, so a network outage loses nothing; only an export made while the receiver is down
// is lost, which is why it runs as a KeepAlive service and doctor checks it.

const (
	// OtelDir is the stream's folder inside the data folder.
	OtelDir = "otel"
	// DefaultMaxBytes caps what the stream keeps on disk while it cannot ship.
	DefaultMaxBytes = 512 << 20
	maxInflated     = 4 * maxBody
)

var signalOf = map[string]string{LogsPath: otelredact.Logs, MetricsPath: otelredact.Metrics, TracesPath: otelredact.Traces}

// Store writes redacted exports to the otel stream.
type Store struct {
	Dir      string // <data dir>/otel
	MaxBytes int64
	// Ignored, when set, is told what each export lost to otel.ignore.
	Ignored   func(otelignore.Dropped)
	mu        sync.Mutex // the kept tool decisions' files, and pruned
	pruned    string     // the UTC day kept decisions were last pruned
	codex     otelignore.CodexSessions
	codexRoot sync.Once
	origins   otelignore.OriginCache
}

// Write stores one export. stored says a line was written; problem names why it was not
// ("" with stored false: an empty export, nothing to keep). dropped marks the size cap.
func (s *Store) Write(path string, body []byte, in http.Header) (status int, kind string, payload []byte, stored, dropped bool, problem string) {
	signal := signalOf[path]
	ct, _, _ := strings.Cut(in.Get("Content-Type"), ";")
	if strings.TrimSpace(ct) != "application/json" {
		return http.StatusUnsupportedMediaType, "text/plain",
			[]byte("livy stores OTLP/JSON only: set the exporter's protocol to http/json (Claude Code) or json (Codex)"),
			false, false, "protobuf refused: an exporter is not set to OTLP/JSON (re-run livy install)"
	}
	if strings.EqualFold(strings.TrimSpace(in.Get("Content-Encoding")), "gzip") {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return http.StatusBadRequest, "text/plain", []byte("bad gzip"), false, false, "unreadable export: bad gzip"
		}
		body, err = io.ReadAll(io.LimitReader(zr, maxInflated+1))
		if err != nil || len(body) > maxInflated {
			return http.StatusRequestEntityTooLarge, "text/plain", []byte("too large"), false, false, "unreadable export: gzip too large or cut short"
		}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return http.StatusBadRequest, "text/plain", []byte("not OTLP/JSON"), false, false, "unreadable export: not JSON"
	}
	s.ignore(signal, doc, in)
	n, err := otelredact.Redact(signal, doc)
	if err != nil {
		// Never stored unredacted: a shape the redactor cannot walk is refused.
		return http.StatusBadRequest, "text/plain", []byte("not an OTLP/JSON export"), false, false,
			"unreadable export: " + err.Error()
	}
	ok := []byte("{}")
	if n == 0 {
		return http.StatusOK, "application/json", ok, false, false, ""
	}
	line, err := json.Marshal(map[string]any{"schema_version": 1, "ts": iso(clock.Now()), "signal": signal, "otlp": doc})
	if err != nil {
		return http.StatusInternalServerError, "text/plain", []byte("could not encode"), false, false, "could not encode: " + err.Error()
	}
	line = append(line, '\n')
	// Before the cap: decisions are small, never shipped, and wanted even when the stream is full.
	var note string
	if signal == otelredact.Logs {
		unchecked, err := s.keepDecisions(doc)
		if err != nil {
			note = "tool decisions not kept: " + err.Error()
		} else if unchecked != nil {
			note = strings.TrimPrefix(note+"; tool decisions kept without the torn-line check: "+unchecked.Error(), "; ")
		}
	}
	if held := s.held(); s.MaxBytes > 0 && held+int64(len(line)) > s.MaxBytes {
		return http.StatusInsufficientStorage, "text/plain", []byte("livy's otel stream is full"), false, true,
			fmt.Sprintf("dropped: the otel stream holds %d bytes waiting to ship, the cap is %d", held, s.MaxBytes)
	}
	done, err := ledger.Append(s.Dir, line)
	if err != nil {
		return http.StatusInternalServerError, "text/plain", []byte("could not write"), false, false, "could not write: " + err.Error()
	}
	if done.Unchecked != nil {
		note = strings.TrimPrefix(note+"; stream line written without the torn-line check: "+done.Unchecked.Error(), "; ")
	}
	// The export is stored and ships; only `livy why`'s copy of its decisions can be lost, and is said.
	return http.StatusOK, "application/json", ok, true, false, note
}

// ignore drops from doc what otel.ignore says must leave no trace. The list is read on each
// export, so a change to it needs no restart.
func (s *Store) ignore(signal string, doc map[string]any, in http.Header) {
	s.codexRoot.Do(func() { s.codex.Root = codex.Root() })
	f := otelignore.Filter{List: otelignore.FromSettings(hook.Settings(), hook.Home()), Origin: s.origins.Origin,
		Session: s.codex.Folder}
	d := f.Apply(signal, doc, in.Get(otelignore.CwdHeader))
	if s.Ignored != nil && (d.Ignored > 0 || d.Unattributed > 0) {
		s.Ignored(d)
	}
}

// keepDecisions keeps the export's tool decisions beside the stream. unchecked is why the
// day file's last line could not be checked for a torn write: they were kept without it.
func (s *Store) keepDecisions(doc map[string]any) (unchecked, err error) {
	ds, err := decisionsIn(doc)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data := filepath.Dir(s.Dir)
	now := clock.Now()
	if len(ds) > 0 {
		unchecked, err = keepDecisions(data, ds, now)
	}
	// Pruned once a day whether or not this export carries decisions.
	if today := now.UTC().Format(decisionDay); s.pruned != today {
		perr := PruneDecisions(data, now)
		if perr == nil {
			s.pruned = today // a failed prune is tried again with the next export
		}
		err = errors.Join(err, perr)
	}
	return unchecked, err
}

// held is the bytes the stream holds waiting to ship: the live file, its segments and spills.
// The shipped segments emit keeps are not waiting, so they never count against the cap.
func (s *Store) held() int64 {
	var n int64
	_ = filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p == filepath.Join(s.Dir, ledger.UploadedDir) {
			return filepath.SkipDir
		}
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// OtelStreamDir is the otel stream's folder for a data folder.
func OtelStreamDir(dataDir string) string { return filepath.Join(dataDir, OtelDir) }

// DecisionsDir is where the receiver keeps Claude Code's and Codex's tool decisions as
// text-free ledger-shaped lines, beside the otel stream and never shipped with it.
const DecisionsDir = "tool-decisions"
