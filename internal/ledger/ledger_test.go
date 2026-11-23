//go:build unix

package ledger

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Guards: the lost event (model check NoLostEvent). The Python hook gives up on the lock
// after 0.2 s and appends to ledger.jsonl unlocked; a rotation that renames, ships and
// deletes the file between that open and write takes the line with it, unshipped.
func TestALineIsNeverWrittenToTheLedgerWithoutTheLock(t *testing.T) {
	dir := t.TempDir()
	line := []byte(`{"event":"turn.ended"}` + "\n")

	if done, err := Append(dir, line); err != nil || done.Spilled {
		t.Fatalf("free lock: spilled=%v err=%v, want the line in the ledger", done.Spilled, err)
	}
	if got := read(t, filepath.Join(dir, FileName)); got != string(line) {
		t.Fatalf("free lock: ledger holds %q, want %q", got, line)
	}

	// The rotation holds the lock: it has renamed the ledger and is shipping it.
	holder, err := os.OpenFile(filepath.Join(dir, LockName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, FileName), filepath.Join(dir, "segment.jsonl")); err != nil {
		t.Fatal(err)
	}
	second := []byte(`{"event":"prompt.submitted"}` + "\n")
	done, err := Append(dir, second)
	if err != nil || !done.Spilled {
		t.Fatalf("held lock: spilled=%v err=%v, want the line spilled", done.Spilled, err)
	}
	// The rotation finishes: the shipped segment is deleted.
	if err := os.Remove(filepath.Join(dir, "segment.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Errorf("held lock: ledger.jsonl was written (stat err %v)", err)
	}
	var found []string
	entries, _ := os.ReadDir(filepath.Join(dir, SpillDir))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			found = append(found, read(t, filepath.Join(dir, SpillDir, e.Name())))
		}
	}
	if len(found) != 1 || found[0] != string(second) {
		t.Errorf("held lock: spill holds %q, want exactly the one line %q", found, second)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// Guards: a line lost when the lock cannot even be opened (a ledger.lock left by another
// user), and two spills in one process publishing over each other.
func TestALineIsSpilledWhenTheLockCannotBeOpened(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, LockName), 0o755); err != nil { // cannot be opened for writing
		t.Fatal(err)
	}
	want := []string{"{\"n\":1}\n", "{\"n\":2}\n", "{\"n\":3}\n"}
	for _, line := range want {
		if done, err := Append(dir, []byte(line)); err != nil || !done.Spilled {
			t.Fatalf("spilled=%v err=%v, want the line spilled", done.Spilled, err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(dir, SpillDir))
	got := map[string]bool{}
	for _, e := range entries {
		got[read(t, filepath.Join(dir, SpillDir, e.Name()))] = true
	}
	for _, line := range want {
		if !got[line] {
			t.Errorf("spill lost %q (holds %v)", line, got)
		}
	}
}

// Guards: a write that fails partway (a full disk) leaving half a line in the ledger, which
// the next append then glues onto, corrupting both lines for good.
func TestAFailedWriteLeavesNoPartialLine(t *testing.T) {
	dir := t.TempDir()
	first := "{\"n\":1}\n"
	if _, err := Append(dir, []byte(first)); err != nil {
		t.Fatal(err)
	}
	real := write
	write = func(f *os.File, b []byte) (int, error) {
		n, _ := real(f, b[:len(b)/2])
		return n, syscall.ENOSPC
	}
	_, err := Append(dir, []byte("{\"n\":2,\"pad\":\"xxxxxxxx\"}\n"))
	write = real
	if err == nil {
		t.Fatal("a failed write reported success")
	}
	third := "{\"n\":3}\n"
	if _, err := Append(dir, []byte(third)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, FileName)); got != first+third {
		t.Errorf("ledger holds %q, want %q", got, first+third)
	}
}

// Guards: a lock file that cannot be opened (an unwritable folder) reported as "lock held", so a
// caller told the user another process had the lock and never named the permission fault.
func TestWithLockNamesALockFileThatCannotBeOpened(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ran := false
	acquired, err := WithLock(filepath.Join(blocker, "sub"), 0, func() error { ran = true; return nil })
	if acquired || ran {
		t.Fatalf("acquired=%v ran=%v, want neither", acquired, ran)
	}
	if err == nil || errors.Is(err, ErrHeld) {
		t.Errorf("err = %v, want the error that kept the lock file from opening", err)
	}
	held, herr := Lock(filepath.Join(dir, LockName), 0)
	if herr != nil {
		t.Fatal(herr)
	}
	defer held()
	if acquired, err := WithLock(dir, 0, func() error { return nil }); acquired || err != nil {
		t.Errorf("a lock another holder has: acquired=%v err=%v, want false and nil", acquired, err)
	}
}
