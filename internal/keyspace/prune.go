package keyspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/0xbew/livy/internal/ledger"
)

// minKeep is the least a prune keeps, whatever the retention: a session working now must not
// lose its file because shipped segments are not kept.
const minKeep = 24 * time.Hour

// Prune deletes the live generation's session, branch and span files that nothing has written
// to for longer than keep (at least a day): their lines are in segments the emitter no longer
// keeps either. The commit, trace and orphan buckets are small and stay.
func Prune(dataDir string, keep time.Duration) error {
	idx := Dir(dataDir)
	release, err := ledger.Lock(filepath.Join(idx, lockName), RebuildWait)
	if err != nil {
		return fmt.Errorf("index lock: %w", err)
	}
	defer release()
	gen, err := current(idx)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-max(keep, minKeep))
	var errs []error
	for _, dir := range []string{ledgerSessions, ledgerBranches, otelSessions} {
		root := filepath.Join(idx, gen, dir)
		werr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil // an unlistable folder is skipped; the rest is pruned
			}
			if fi, err := d.Info(); err == nil && fi.ModTime().Before(cutoff) {
				errs = append(errs, os.Remove(p))
			}
			return nil
		})
		if werr != nil && !errors.Is(werr, fs.ErrNotExist) {
			errs = append(errs, werr)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("index prune: %w", err)
	}
	return nil
}
