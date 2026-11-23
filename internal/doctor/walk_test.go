package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

// Guards: doctor under-reporting how long data has waited because a segments folder could not be
// listed. The walk says it could not read, so the caller never reads "nothing waiting" from it.
func TestOldestWaitingSaysAFolderItCannotList(t *testing.T) {
	dir := t.TempDir()
	seg := filepath.Join(dir, "segments")
	if err := os.MkdirAll(seg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(seg, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(seg, 0o700)
	if _, err := os.ReadDir(seg); err == nil {
		t.Skip("the folder is listable here (running as root)")
	}
	if _, _, unreadable := oldestWaiting(dir); !unreadable {
		t.Error("an unlistable segments folder read as nothing waiting")
	}
}
