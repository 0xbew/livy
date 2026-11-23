package install

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Guards: a failed mint (gcloud cannot act as the writer) printing a header anyway, which
// turns the cause into an opaque 401 at the collector, or failing with nothing in the ledger,
// which ships no trace of why export stopped.
func TestGcloudHeadersFailureIsSaidNeverSent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LIVY_DIR", filepath.Join(home, ".livy"))
	gcloud := filepath.Join(home, "gcloud")
	if err := os.WriteFile(gcloud, []byte("#!/bin/sh\necho 'ERROR: Permission iam.serviceAccounts.getOpenIdToken denied' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".livy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"sink": {"type": "gcs", "bucket": "b", "writer_sa": "w@p.iam.gserviceaccount.com", "gcloud": "` + gcloud + `"}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	st := `{"env": {"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example"}}`
	if err := os.WriteFile(filepath.Join(dir, "install.json"), []byte(st), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := OtelHeaders([]string{"--gcloud"}, &out, &errOut); code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q on a failed mint, want nothing", out.String())
	}
	ledger, _ := os.ReadFile(filepath.Join(dir, "ledger.jsonl"))
	if !strings.Contains(string(ledger), `"event":"ledger.error"`) || !strings.Contains(string(ledger), "getOpenIdToken denied") {
		t.Errorf("ledger holds no ledger.error naming the cause:\n%s", ledger)
	}
}
