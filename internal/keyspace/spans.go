package keyspace

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/0xbew/livy/internal/otelspan"
)

// sessionKeys are the span attributes that name a span's session, as `livy profile` reads them:
// Claude Code puts session.id on every span; Codex names its conversation on the turn and a few
// others only.
var sessionKeys = []string{"session.id", "thread_id", "conversation.id", "thread.id"}

// compactSpan is one span as the index keeps it: exactly what otelspan reads from an export.
type compactSpan struct {
	Trace  string            `json:"t"`
	ID     string            `json:"s"`
	Parent string            `json:"p,omitempty"`
	Name   string            `json:"n"`
	Start  int64             `json:"b"`
	End    int64             `json:"e"`
	Attrs  map[string]string `json:"a,omitempty"`
}

// tracePair is one line of a trace bucket: a trace names a session.
type tracePair struct {
	Trace   string `json:"t"`
	Session string `json:"s"`
}

// sessionsOf is the sessions a span names, each once.
func sessionsOf(s otelspan.Span) []string {
	var out []string
	for _, k := range sessionKeys {
		if v := s.Attrs[k]; v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// spanLine is s as one index line.
func spanLine(s otelspan.Span) ([]byte, error) {
	b, err := json.Marshal(compactSpan{Trace: s.Trace, ID: s.ID, Parent: s.Parent, Name: s.Name,
		Start: s.Start.UnixNano(), End: s.End.UnixNano(), Attrs: s.Attrs})
	if err != nil {
		return nil, fmt.Errorf("encode span: %w", err)
	}
	return append(b, '\n'), nil
}

// DecodeSpan reads one span line of the index.
func DecodeSpan(line []byte) (otelspan.Span, error) {
	var c compactSpan
	if err := json.Unmarshal(line, &c); err != nil {
		return otelspan.Span{}, fmt.Errorf("index span line: %w", err)
	}
	attrs := c.Attrs
	if attrs == nil {
		attrs = map[string]string{}
	}
	return otelspan.Span{Trace: c.Trace, ID: c.ID, Parent: c.Parent, Name: c.Name,
		Start: time.Unix(0, c.Start).UTC(), End: time.Unix(0, c.End).UTC(), Attrs: attrs}, nil
}

// DecodeTracePair reads one line of a trace bucket.
func DecodeTracePair(line []byte) (trace, session string, err error) {
	var p tracePair
	if err := json.Unmarshal(line, &p); err != nil {
		return "", "", fmt.Errorf("index trace line: %w", err)
	}
	return p.Trace, p.Session, nil
}

// spanFiles is the index lines an export's spans make, by file relative to a generation: each
// span in the file of every session it names (or its trace's orphan bucket when it names none),
// and one trace-bucket line per trace and session the export names.
func spanFiles(spans []otelspan.Span) (map[string][]byte, error) {
	out := map[string][]byte{}
	paired := map[tracePair]bool{}
	for _, s := range spans {
		line, err := spanLine(s)
		if err != nil {
			return nil, err
		}
		names := sessionsOf(s)
		if len(names) == 0 {
			f := OrphanBucket(s.Trace)
			out[f] = append(out[f], line...)
			continue
		}
		for _, id := range names {
			f := SpanFile(id)
			out[f] = append(out[f], line...)
			p := tracePair{Trace: s.Trace, Session: id}
			if paired[p] {
				continue
			}
			paired[p] = true
			pl, err := json.Marshal(p)
			if err != nil {
				return nil, fmt.Errorf("encode trace line: %w", err)
			}
			tb := TraceBucket(s.Trace)
			out[tb] = append(append(out[tb], pl...), '\n')
		}
	}
	return out, nil
}

// ledgerLines is the index lines a run of ledger lines makes, by file relative to a generation.
// Lines that are not JSON objects, or that no key claims, are left out.
func ledgerLines(data []byte) map[string][]byte {
	out := map[string][]byte{}
	for _, line := range splitLines(data) {
		k, ok := KeysOf(line)
		if !ok {
			continue
		}
		for _, f := range ledgerFiles(k) {
			out[f] = append(append(out[f], line...), '\n')
		}
	}
	return out
}

// otelLines is the index lines a run of otel stream lines makes; a line that cannot be read is
// counted in bad and left out.
func otelLines(data []byte) (files map[string][]byte, bad int) {
	files = map[string][]byte{}
	for _, line := range splitLines(data) {
		spans, err := otelspan.Decode(line)
		if err != nil {
			bad++
			continue
		}
		got, err := spanFiles(spans)
		if err != nil {
			bad++
			continue
		}
		for f, b := range got {
			files[f] = append(files[f], b...)
		}
	}
	return files, bad
}

// splitLines is data's non-empty lines, without their newlines.
func splitLines(data []byte) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		i := 0
		for i < len(data) && data[i] != '\n' {
			i++
		}
		if line := trimCR(data[:i]); len(line) > 0 {
			out = append(out, line)
		}
		if i == len(data) {
			break
		}
		data = data[i+1:]
	}
	return out
}

func trimCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}

// genPath is rel inside generation gen of the index folder idx.
func genPath(idx, gen, rel string) string { return filepath.Join(idx, gen, rel) }
