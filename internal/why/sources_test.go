package why

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xbew/livy/internal/emit"
	"github.com/0xbew/livy/internal/ledger"
)

// Guards: an on-call read a report missing the local events in a spill or segments folder that
// exists but cannot be read, with nothing saying so and exit 0. Only a folder that is not there
// is skipped quietly.
func TestUnreadableLocalFolderIsReported(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LIVY_DIR", dir)
	spill := filepath.Join(dir, ledger.SpillDir)
	if err := os.Mkdir(spill, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(spill, 0o700) })
	if _, err := os.ReadDir(spill); err == nil {
		t.Skip("a 0000 folder is listable here (root, or a file system without modes)")
	}
	var out, errb bytes.Buffer
	code := Main([]string{"HEAD"}, &out, &errb)
	if code != 1 {
		t.Errorf("why HEAD with an unreadable %s/ exited %d, want 1; stderr %q", ledger.SpillDir, code, errb.String())
	}
	if !strings.Contains(out.String(), "spill could not be read") {
		t.Errorf("why HEAD output does not name the unreadable spill folder:\n%s", out.String())
	}
	if strings.Contains(out.String(), "unshipped segments could not be read") {
		t.Errorf("why HEAD reports an absent %s/ as unreadable:\n%s", emit.SegmentsDir, out.String())
	}
}
