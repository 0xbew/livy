package emit

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xbew/livy/internal/config"
	"github.com/0xbew/livy/internal/oldnames"
	"github.com/0xbew/livy/internal/prompts"
)

// Guards: two keys for one segment (a retry would then ship it twice) and a dt= taken
// from the upload clock (a segment rotated either side of midnight lands on the wrong
// day). The hashes and the aware-ts days are the Python emitter's own answers for the
// same bytes (emit.segment_id, emit.partition_date); the naive-only row is contract rule 9.
func TestKey(t *testing.T) {
	rotated := time.Date(2026, 11, 2, 23, 59, 59, 0, time.UTC)
	for _, c := range []struct {
		name, raw, handle, want string
	}{
		{"an offset ts is converted to its UTC day",
			`{"event":"turn.ended","repo":"acme/w","schema_version":1,"ts":"2026-11-02T23:30:00-05:00"}` + "\n", "",
			"source=agent-hooks/dt=2026-11-03/bca80f91ea51e35b.jsonl.gz"},
		{"a naive ts is skipped for the first aware one; the handle is in the key",
			`{"event":"turn.ended","repo":"acme/w","schema_version":1,"ts":"2026-11-02T10:00:00"}` + "\n" +
				`{"event":"turn.ended","repo":"acme/w","schema_version":1,"ts":"2026-11-01T22:00:00Z"}` + "\n", "octocat",
			"source=agent-hooks/dt=2026-11-01/handle=octocat/7a9b2ebc8a837da7.jsonl.gz"},
		{"no aware ts: the day it was rotated, not the upload clock",
			`{"event":"turn.ended","repo":"acme/w","schema_version":1,"ts":"2026-11-02T10:00:00"}` + "\n", "",
			"source=agent-hooks/dt=2026-11-02/a80be15cf4107d09.jsonl.gz"},
	} {
		got, _, err := Key([]byte(c.raw), rotated, c.handle)
		if err != nil || got != c.want {
			t.Errorf("%s: Key = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

// fakeSink stands in for a bucket that refuses until told to accept.
type fakeSink struct {
	accept  bool
	objects map[string][]byte
}

func (f *fakeSink) URL(key string) string { return "fake://" + key }
func (f *fakeSink) Put(_ context.Context, key string, body []byte) error {
	if !f.accept {
		return errors.New("HTTP 403: denied")
	}
	f.objects[key] = body
	return nil
}

// Guards: the Python error spiral (each refused pass minted one more error-only segment,
// so an outage grew the backlog without bound) and losing lines across a refusal. While
// the sink refuses, a pass keeps everything and rotates at most once per UTC day; once it
// accepts, every line written before lands in exactly one object.
func TestARefusingSinkDoesNotGrowTheBacklog(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, "data")
	t.Setenv("HOME", home)
	t.Setenv("LIVY_DIR", data)
	day1 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	at := func(t0 time.Time) { t.Setenv("LIVY_CONFORMANCE_NOW", strconv.FormatInt(t0.Unix(), 10)) }
	written := []string{
		`{"event":"session.started","repo":"acme/w","schema_version":1,"session_id":"s-1","ts":"2026-11-03T11:00:00+00:00"}`,
		`{"event":"turn.ended","repo":"acme/w","schema_version":1,"session_id":"s-1","ts":"2026-11-03T11:05:00+00:00"}`,
	}
	if err := os.MkdirAll(data, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "ledger.jsonl"), []byte(strings.Join(written, "\n")+"\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	sink := &fakeSink{objects: map[string][]byte{}}
	pass := func() { Run(context.Background(), nil, nil, io.Discard, func() (Sink, error) { return sink, nil }) }

	at(day1)
	pass()
	pass()
	pass()
	if n := countSegments(t, data); n != 1 {
		t.Fatalf("after three refused passes on one day: %d segments, want 1", n)
	}
	at(day1.Add(24 * time.Hour))
	pass()
	pass()
	if n := countSegments(t, data); n != 2 {
		t.Fatalf("after refused passes on the next day: %d segments, want 2", n)
	}

	sink.accept = true
	pass()
	if n := countSegments(t, data); n != 0 {
		t.Fatalf("after an accepted pass: %d segments still waiting, want 0", n)
	}
	seen := map[string]int{}
	for _, body := range sink.objects {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(zr)
		for sc.Scan() {
			seen[sc.Text()]++
		}
	}
	for _, line := range written {
		if seen[line] != 1 {
			t.Errorf("line shipped %d times, want once: %s", seen[line], line)
		}
	}
}

func countSegments(t *testing.T, data string) int {
	t.Helper()
	n := 0
	_ = filepath.Walk(filepath.Join(data, SegmentsDir), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".jsonl") {
			n++
		}
		return nil
	})
	return n
}

// Guards: a machine with no sink configured appended a ledger.error on every throttled pass
// (every prompt, turn and tool call past the throttle), growing the ledger without bound and
// failing doctor on a fresh install. Not configured is a state doctor reports, not an error.
func TestNoSinkWritesNoLedgerError(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, "data")
	t.Setenv("HOME", home)
	t.Setenv("LIVY_DIR", data)
	for range 3 {
		Run(context.Background(), nil, nil, io.Discard, func() (Sink, error) { return nil, ErrNoSink })
	}
	if raw, _ := os.ReadFile(filepath.Join(data, "ledger.jsonl")); bytes.Contains(raw, []byte("ledger.error")) {
		t.Errorf("three passes with no sink wrote:\n%s", raw)
	}
	// A half-written sink section is broken, not absent: it must still fail loudly.
	half := config.Resolve(nil, nil, map[string]any{"sink.bucket": "b"}, nil)
	if _, err := FromSettings(half); err == nil || errors.Is(err, ErrNoSink) {
		t.Errorf("sink.bucket with no sink.type: got %v, want an error that is not ErrNoSink", err)
	}
	// Guards: a refused managed file holds the user's sinks back; reading that as no sink kept
	// the data local with no ledger.error, while the admin's upload target was never used.
	refused := config.Load(config.Inputs{Managed: []byte(`{"sink":`),
		User: []byte(`{"sink":{"type":"local","path":"/tmp/a"},"prompts":{"sink":{"type":"local","path":"/tmp/b"}}}`)})
	if _, err := FromSettings(refused); err == nil || errors.Is(err, ErrNoSink) {
		t.Errorf("sink with the managed file refused: got %v, want an error that is not ErrNoSink", err)
	}
	if _, err := PromptSinkFromSettings(refused); err == nil || errors.Is(err, ErrNoPromptSink) {
		t.Errorf("prompt sink with the managed file refused: got %v, want an error that is not ErrNoPromptSink", err)
	}
}

// Guards: the agent work behind a commit being unreadable on the author's laptop minutes after
// it happened. emit deleted each segment once the sink had it, so `livy why` on a machine with a
// GCS or S3 sink saw only the live ledger. An uploaded segment is kept under uploaded/ for
// emit.keep_uploaded_days (30 by default), counted from its upload, and pruned after that.
func TestUploadedSegmentsAreKeptThenPruned(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, "data")
	t.Setenv("HOME", home)
	t.Setenv("LIVY_DIR", data)
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	t.Setenv("LIVY_CONFORMANCE_NOW", strconv.FormatInt(now.Unix(), 10))
	line := `{"event":"turn.ended","repo":"acme/w","schema_version":1,"session_id":"s-1","ts":"2026-11-12T11:00:00+00:00"}`
	kept := filepath.Join(data, UploadedDir, "_no-handle")
	if err := os.MkdirAll(kept, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, age := range map[string]time.Duration{"old.jsonl": 31 * 24 * time.Hour, "recent.jsonl": 29 * 24 * time.Hour} {
		p := filepath.Join(kept, name)
		if err := os.WriteFile(p, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(data, "ledger.jsonl"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sink := &fakeSink{accept: true, objects: map[string][]byte{}}
	Run(context.Background(), nil, nil, io.Discard, func() (Sink, error) { return sink, nil })

	if len(sink.objects) != 1 || countSegments(t, data) != 0 {
		t.Fatalf("got %d objects and %d waiting segments, want the ledger shipped", len(sink.objects), countSegments(t, data))
	}
	var names []string
	_ = filepath.Walk(filepath.Join(data, UploadedDir), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			names = append(names, filepath.Base(p))
		}
		return nil
	})
	var shipped, recent, old bool
	for _, n := range names {
		switch {
		case n == "recent.jsonl":
			recent = true
		case n == "old.jsonl":
			old = true
		case strings.HasSuffix(n, ".jsonl"):
			shipped = true
		}
	}
	if !shipped || !recent || old {
		t.Errorf("uploaded/ holds %v, want the segment just shipped and recent.jsonl, not old.jsonl", names)
	}
}

// Guards: `livy profile` reading 0 OTEL spans on a machine whose receiver had stored and shipped
// dozens of span exports that day. emit kept shipped ledger segments but deleted each shipped
// segment of the otel stream, so with a GCS or S3 sink no span outlived the next emit.
func TestShippedOtelSegmentsAreKept(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, "data")
	t.Setenv("HOME", home)
	t.Setenv("LIVY_DIR", data)
	t.Setenv("LIVY_CONFORMANCE_NOW", strconv.FormatInt(time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC).Unix(), 10))
	otel := filepath.Join(data, "otel")
	if err := os.MkdirAll(otel, 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"otlp":{"resourceSpans":[]},"ts":"2026-11-12T11:00:00Z"}`
	if err := os.WriteFile(filepath.Join(otel, "ledger.jsonl"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sink := &fakeSink{accept: true, objects: map[string][]byte{}}
	Run(context.Background(), nil, nil, io.Discard, func() (Sink, error) { return sink, nil })

	if len(sink.objects) != 1 {
		t.Fatalf("got %d objects, want the otel segment shipped", len(sink.objects))
	}
	kept, _ := filepath.Glob(filepath.Join(otel, UploadedDir, "*", "*.jsonl"))
	if len(kept) != 1 {
		t.Errorf("otel/%s holds %v after shipping, want the shipped segment kept", UploadedDir, kept)
	}
}

// Guards: a shipped segment's local copy lost when it cannot be kept. If uploaded/ cannot be
// made (here a file stands in its place), the segment stays in segments/; the next pass finds
// the object already in the sink and tries to keep it again.
func TestASegmentThatCannotBeKeptStaysWaiting(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, "data")
	t.Setenv("HOME", home)
	t.Setenv("LIVY_DIR", data)
	t.Setenv("LIVY_CONFORMANCE_NOW", strconv.FormatInt(time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC).Unix(), 10))
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, UploadedDir), []byte("not a folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	line := `{"event":"turn.ended","repo":"acme/w","schema_version":1,"session_id":"s-1","ts":"2026-11-12T11:00:00+00:00"}`
	if err := os.WriteFile(filepath.Join(data, "ledger.jsonl"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sink := &fakeSink{accept: true, objects: map[string][]byte{}}
	Run(context.Background(), nil, nil, io.Discard, func() (Sink, error) { return sink, nil })
	if len(sink.objects) != 1 || countSegments(t, data) != 1 {
		t.Errorf("got %d objects and %d waiting segments, want it shipped and still waiting to be kept",
			len(sink.objects), countSegments(t, data))
	}
}

// Guards: emit --extract always skipped prompt capture, and only Claude Code's SessionStart hook
// ran it, so a machine with only Codex or Amp never captured a prompt and elsewhere capture waited
// for the next Claude Code session. The daily job and the throttled flush run the pass too.
func TestEmitExtractRunsThePromptPass(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, "data")
	t.Setenv("HOME", home)
	t.Setenv("LIVY_DIR", data)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	if k := oldnames.RecorderEnv("TRANSCRIPT_ROOTS"); k != "" {
		t.Setenv(k, "")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	Run(context.Background(), []string{"--extract"}, nil, io.Discard, func() (Sink, error) { return nil, ErrNoSink })
	if _, err := os.Stat(filepath.Join(data, prompts.StateName)); err != nil {
		t.Errorf("emit --extract left no prompt-capture state (%v): the prompt pass did not run", err)
	}
}
