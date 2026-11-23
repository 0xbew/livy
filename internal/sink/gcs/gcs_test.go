//go:build unix

package gcs

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/0xbew/livy/internal/oldnames"
)

// Guards: a retry replacing an object (write-once is our precondition, not the bucket's
// default), and a segment deleted locally on an answer that does not prove the bucket has
// it. 412 alone means "already there"; a refusal (403) must stay a failure so the segment
// is kept. The answer bodies are the shapes fake-gcs-server and GCS return.
func TestPutIsCreateOnlyAndOnly412IsAlreadyThere(t *testing.T) {
	status := http.StatusOK
	var precondition, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		precondition, auth = r.URL.Query().Get("ifGenerationMatch"), r.Header.Get("Authorization")
		w.WriteHeader(status)
		switch status {
		case http.StatusOK:
			_, _ = w.Write([]byte(`{"kind":"storage#object","name":"source=agent-hooks/dt=2026-11-03/k.jsonl.gz"}`))
		case http.StatusPreconditionFailed:
			_, _ = w.Write([]byte(`{"error":{"code":412,"errors":[{"reason":"conditionNotMet"}]}}`))
		}
	}))
	defer srv.Close()
	s, err := New(Config{Bucket: "gs://b/", Endpoint: srv.URL, Auth: "command", TokenCommand: []string{"printf", "tok-1"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		status            int
		wantOK, wantExist bool
	}{{http.StatusOK, true, false}, {http.StatusPreconditionFailed, false, true}, {http.StatusForbidden, false, false}} {
		status = c.status
		err := s.Put(context.Background(), "source=agent-hooks/dt=2026-11-03/k.jsonl.gz", []byte("x"))
		if (err == nil) != c.wantOK || errors.Is(err, fs.ErrExist) != c.wantExist {
			t.Errorf("HTTP %d: Put = %v; want ok=%v, already-there=%v", c.status, err, c.wantOK, c.wantExist)
		}
		if precondition != "0" || auth != "Bearer tok-1" {
			t.Errorf("HTTP %d: request had ifGenerationMatch=%q, Authorization=%q; want 0 and the minted token", c.status, precondition, auth)
		}
	}
}

// fakeGcloud is an executable that records its arguments and prints a token.
func fakeGcloud(t *testing.T) (path, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	path = filepath.Join(dir, "gcloud")
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\necho tok-gcloud\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, argsFile
}

// Guards: a machine migrating from the Python tool, whose writer_sa "active"
// means "use the signed-in account as it is". Read as a service account's name, every
// upload would ask gcloud to impersonate an account called "active" and fail.
func TestWriterActiveMeansTheAccountAsIs(t *testing.T) {
	g, argsFile := fakeGcloud(t)
	s, err := New(Config{Bucket: "b", Auth: "gcloud", Gcloud: g, WriterSA: "active", Account: "dev@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.accessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(argsFile)
	if want := "auth print-access-token --account=dev@example.test\n"; string(got) != want {
		t.Errorf("gcloud was run as %q, want %q", got, want)
	}
}

// Guards: the Python tool's GCLOUD variable, which names the gcloud to run and is
// not second-guessed: set to something that cannot run, uploads fail loudly rather than
// quietly using another gcloud from PATH (a different install, a different account).
func TestGcloudOverrideIsTheOnlyGcloudTried(t *testing.T) {
	override := oldnames.RecorderEnv("GCLOUD")
	if override == "" {
		t.Skip("this build has no Python tool's names")
	}
	onPath, argsFile := fakeGcloud(t)
	t.Setenv("PATH", filepath.Dir(onPath)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(override, filepath.Join(t.TempDir(), "no-such-gcloud"))
	s, err := New(Config{Bucket: "b", Auth: "gcloud"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.accessToken(context.Background()); err == nil {
		t.Errorf("a missing %s still minted a token", override)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Errorf("the gcloud on PATH was run although %s names another", override)
	}

	other, otherArgs := fakeGcloud(t)
	t.Setenv(override, other)
	s, _ = New(Config{Bucket: "b", Auth: "gcloud"})
	if _, err := s.accessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(otherArgs); err != nil {
		t.Errorf("%s was not the gcloud that ran", override)
	}
}
