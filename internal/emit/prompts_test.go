package emit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xbew/scrub"

	"github.com/0xbew/livy/internal/config"
)

// A denylist that grows between the write and the upload refuses at upload: the record is held
// aside, named by id and rule and never by text, and the rest of its segment still ships.
func TestAGrownDenylistHoldsOneRecordAndShipsTheRest(t *testing.T) {
	dir := t.TempDir()
	seg := filepath.Join(dir, "seg.jsonl")
	body := `{"prompt_id":"prompt:a","prompt":"fine"}` + "\n" + `{"prompt_id":"prompt:b","prompt":"ask Zebulon Quux"}` + "\n"
	if err := os.WriteFile(seg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := scrub.ParseDenylist([]byte(`{"identities":[{"literal":"Zebulon Quux","role":"owner"}],"vendors":[]}`), 2)
	if err != nil {
		t.Fatal(err)
	}
	var said bytes.Buffer
	if !holdHits(seg, filepath.Join(dir, "rej"), d, &said) {
		t.Fatal("nothing left to ship")
	}
	left, _ := os.ReadFile(seg)
	if strings.Contains(string(left), "Quux") || !strings.Contains(string(left), "prompt:a") {
		t.Errorf("segment after the hold: %s", left)
	}
	if held, _ := filepath.Glob(filepath.Join(dir, "rej", "held-*.json")); len(held) != 1 {
		t.Errorf("held %d records", len(held))
	}
	if !strings.Contains(said.String(), "prompt:b") || strings.Contains(said.String(), "Quux") {
		t.Errorf("report: %s", said.String())
	}
}

// Prompt text never goes through the ledger's sink.
func TestAPromptSinkNamingTheLedgersPlaceIsRefused(t *testing.T) {
	user := map[string]any{"sink.type": "local", "sink.path": "/data", "prompts.sink.type": "local", "prompts.sink.path": "/data"}
	if _, err := PromptSinkFromSettings(config.Resolve(nil, nil, user, nil)); err != ErrSamePromptSink {
		t.Errorf("same place: %v", err)
	}
	user["prompts.sink.path"] = "/prompts"
	if _, err := PromptSinkFromSettings(config.Resolve(nil, nil, user, nil)); err != nil {
		t.Errorf("another place: %v", err)
	}
	if _, err := PromptSinkFromSettings(config.Resolve(nil, nil, map[string]any{"sink.type": "local", "sink.path": "/data"}, nil)); err != ErrNoPromptSink {
		t.Errorf("no prompt sink: %v", err)
	}
}

// A prompt sink in the ledger's bucket, or a folder inside the ledger's folder, is the ledger's
// place however its scheme, case, prefix or slashes are written.
func TestAPromptSinkOverlappingTheLedgersPlaceIsRefused(t *testing.T) {
	rows := []struct {
		name string
		user map[string]any
	}{
		{"same bucket, other prefix", map[string]any{"sink.type": "gcs", "sink.bucket": "Ledger", "prompts.sink.type": "gcs", "prompts.sink.bucket": "ledger", "prompts.sink.prefix": "prompts"}},
		{"trailing slash", map[string]any{"sink.type": "local", "sink.path": "/data/", "prompts.sink.type": "local", "prompts.sink.path": "/data"}},
		{"folder inside", map[string]any{"sink.type": "local", "sink.path": "/data", "prompts.sink.type": "local", "prompts.sink.path": "/data/prompts"}},
		{"ledger inside prompts", map[string]any{"sink.type": "local", "sink.path": "/data/ledger", "prompts.sink.type": "local", "prompts.sink.path": "/data"}},
	}
	for _, r := range rows {
		if _, err := PromptSinkFromSettings(config.Resolve(nil, nil, r.user, nil)); err != ErrSamePromptSink {
			t.Errorf("%s: %v", r.name, err)
		}
	}
}
