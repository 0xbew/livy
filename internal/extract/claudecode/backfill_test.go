//go:build unix

package claudecode

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0xbew/livy/internal/extract"
)

func usageBySource(t *testing.T, data, id string) map[string]int {
	t.Helper()
	got := map[string]int{}
	for _, l := range ledgerLines(t, data) {
		if l["event"] == "usage.api_call" && l["message_id"] == id {
			src, _ := l["source"].(string)
			got[src]++
		}
	}
	return got
}

// TestBackfillLeavesTheExtractorsStateAlone guards the live extractor from a backfill: a
// backfill that advanced an offset or recorded a line in the written log would make the
// extractor skip that call's live usage line for good.
func TestBackfillLeavesTheExtractorsStateAlone(t *testing.T) {
	repo, root, data := setup(t)
	settled := time.Now().Add(time.Hour)
	writeLines(t, filepath.Join(root, "-p", "s1.jsonl"), assistant(repo, "m-1", 3, "2026-11-01T10:00:00.000Z"))
	backfill := func() {
		Backfill(Options{Roots: []string{root}, Dir: data, Stderr: io.Discard, Now: settled})
	}

	backfill() // before the extractor has ever run
	for _, name := range []string{stateName, extract.WrittenName} {
		if _, err := os.Stat(filepath.Join(data, name)); err == nil {
			t.Errorf("a backfill with no extractor state created %s", name)
		}
	}
	var line map[string]any
	for _, l := range ledgerLines(t, data) {
		if l["event"] == "usage.api_call" {
			line = l
		}
	}
	if line == nil || line["source"] != BackfillSource || line["target_via"] != "session" ||
		line["target_repo"] != "acme/app" || line["target_branch"] != "main" {
		t.Fatalf("backfill wrote %v, want m-1 with source %s and its session target acme/app main", line, BackfillSource)
	}

	run(t, root, data, settled)
	if got := usageBySource(t, data, "m-1"); got["transcript"] != 1 || got[BackfillSource] != 1 || len(got) != 2 {
		t.Fatalf("m-1 written by source %v after the extractor ran, want one live copy and one backfill copy", got)
	}

	snapshot := func() [2]string {
		var out [2]string
		for i, name := range []string{stateName, extract.WrittenName} {
			b, err := os.ReadFile(filepath.Join(data, name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			out[i] = string(b)
		}
		return out
	}
	before := snapshot()
	backfill()
	if after := snapshot(); after != before {
		t.Errorf("a backfill changed the extractor's state or written log")
	}
	if got := usageBySource(t, data, "m-1"); got[BackfillSource] != 2 {
		t.Errorf("a second backfill left %d backfill copies of m-1, want 2 (it appends everything again)", got[BackfillSource])
	}
}
