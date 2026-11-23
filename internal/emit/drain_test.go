package emit

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xbew/livy/internal/config"
	"github.com/0xbew/livy/internal/hook"
)

// Guards: records a hosted orb pushed to its git sink repository never reaching the install's
// own sink, reaching it under a key other than the one emit gives the same bytes, a malformed
// segment copied in silently, and a second run copying or reporting again. The repository is a
// real one filled by the git sink, as an orb fills it.
func TestDrainCopiesTheGitSinkIntoTheConfiguredSink(t *testing.T) {
	tmp := t.TempDir()
	home, out := filepath.Join(tmp, "home"), filepath.Join(tmp, "bucket")
	t.Setenv("HOME", home)
	t.Setenv("LIVY_DIR", filepath.Join(tmp, "data"))
	repo := filepath.Join(tmp, "orb-sink.git")
	if b, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, b)
	}
	writeConfig(t, home, `{"drain": {"source": {"type": "git", "remote": "`+repo+`"}}, "sink": {"type": "local", "path": "`+out+`"}}`)
	good := []byte(`{"event":"turn.ended","repo":"acme/w","schema_version":1,"ts":"2026-11-01T22:00:00Z"}` + "\n")
	bad := []byte(`{"event":"turn.ended","repo":"acme/w","schema_version":1,"ts":"2026-11-01T22:00:00Z","planted":1}` + "\n")
	goodKey := pushSegment(t, repo, good, "orb-user")
	badKey := pushSegment(t, repo, bad, "orb-user")

	sinkFor := func() (Sink, error) { return FromSettings(hook.Settings()) }
	var stderr bytes.Buffer
	if code := Drain(t.Context(), nil, &stderr, sinkFor); code != 0 {
		t.Fatalf("Drain = %d, want 0; stderr: %s", code, stderr.String())
	}
	if got := gunzipFile(t, filepath.Join(out, goodKey)); !bytes.Equal(got, good) {
		t.Errorf("sink %s = %q, want the orb's segment %q; stderr: %s", goodKey, got, good, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(out, badKey)); err == nil {
		t.Errorf("the malformed segment %s reached the sink", badKey)
	}
	errs := ledgerErrors(t)
	if len(errs) != 1 || !strings.Contains(errs[0], badKey) {
		t.Errorf("ledger.error lines = %q, want one naming %s", errs, badKey)
	}

	stderr.Reset()
	if code := Drain(t.Context(), nil, &stderr, sinkFor); code != 0 {
		t.Fatalf("second Drain = %d, want 0; stderr: %s", code, stderr.String())
	}
	if again := ledgerErrors(t); len(again) != 1 {
		t.Errorf("a second run reported again: ledger.error lines = %q", again)
	}
	if !strings.Contains(stderr.String(), "0 copied") {
		t.Errorf("second run stderr = %q, want 0 copied", stderr.String())
	}
}

func writeConfig(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".livy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// pushSegment pushes raw through a git sink built as the orb's emit builds it, and returns its key.
func pushSegment(t *testing.T, repo string, raw []byte, handle string) string {
	t.Helper()
	key, _, err := Key(raw, time.Now(), handle)
	if err != nil {
		t.Fatal(err)
	}
	s, err := FromSettings(config.Load(config.Inputs{User: []byte(`{"sink": {"type": "git", "remote": "` + repo + `"}}`)}))
	if err != nil {
		t.Fatal(err)
	}
	body, err := Gzip(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), key, body); err != nil {
		t.Fatal(err)
	}
	return key
}

func gunzipFile(t *testing.T, path string) []byte {
	t.Helper()
	gz, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ledgerErrors(t *testing.T) []string {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(os.Getenv("LIVY_DIR"), "ledger.jsonl"))
	var out []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.Contains(line, `"ledger.error"`) {
			out = append(out, line)
		}
	}
	return out
}
