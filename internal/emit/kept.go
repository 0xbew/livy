package emit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/hook"
)

// KeepUploadedKey is the setting for how many days a shipped segment (the ledger's or the otel
// stream's) stays under its UploadedDir, counted from its upload. 0 keeps none.
const KeepUploadedKey = "emit.keep_uploaded_days"

// defaultKeepDays is the retention when the setting is absent.
const defaultKeepDays = 30

// maxKeepDays bounds the setting: a larger count of days overflows a time.Duration, which would
// turn negative and prune every kept segment.
const maxKeepDays = 36500

const day = 24 * time.Hour

// keepUploaded is how long shipped segments are kept, and why the setting could not be used
// ("" when it could).
func keepUploaded() (time.Duration, string) {
	v, ok := hook.Settings().Settings[KeepUploadedKey]
	if !ok {
		return defaultKeepDays * day, ""
	}
	var days int64
	var err error
	switch x := v.Value.(type) {
	case json.Number:
		days, err = x.Int64()
	case string:
		days, err = strconv.ParseInt(x, 10, 64)
	default:
		err = fmt.Errorf("not a number")
	}
	if err != nil || days < 0 || days > maxKeepDays {
		return defaultKeepDays * day, fmt.Sprintf("%s from %s is not a whole number of days from 0 to %d (%v); keeping %d", KeepUploadedKey, v.Layer, maxKeepDays, v.Value, defaultKeepDays)
	}
	return time.Duration(days) * day, ""
}

// keep moves a shipped segment under UploadedDir, in its handle's folder, and stamps it with
// the upload time the retention counts from; when it cannot be moved it stays where it is. With
// no retention it is deleted.
func (p *pass) keep(seg string) {
	if p.keepFor > 0 {
		dest := filepath.Join(p.dir, UploadedDir, filepath.Base(filepath.Dir(seg)))
		if filepath.Dir(seg) == filepath.Join(p.dir, SegmentsDir) {
			dest = filepath.Join(p.dir, UploadedDir, anonymous)
		}
		err := os.MkdirAll(dest, 0o700)
		target := freeName(filepath.Join(dest, filepath.Base(seg)))
		if err == nil {
			err = os.Rename(seg, target)
		}
		if err == nil {
			now := clock.Now()
			err = os.Chtimes(target, now, now)
			if err != nil {
				p.say("kept %s but could not stamp its upload time (%v); it is pruned by its rotation time", filepath.Base(seg), err)
			}
			return
		}
		// Left in segments/: the next pass finds the object already in the sink and tries again,
		// so the local copy the retention promises is never lost to a failed move.
		p.say("shipped %s but could not keep a copy (%v); it stays in segments/ and the next run tries again", filepath.Base(seg), err)
		return
	}
	if err := os.Remove(seg); err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.say("shipped %s but could not delete it (%v); the next run's upload is a no-op", filepath.Base(seg), err)
	}
}

// prune deletes kept segments uploaded longer ago than the retention.
func (p *pass) prune() {
	cutoff := clock.Now().Add(-p.keepFor)
	failed := 0
	_ = filepath.WalkDir(filepath.Join(p.dir, UploadedDir), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // a folder that cannot be read keeps its files; nothing is lost
		}
		info, err := d.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			return nil //nolint:nilerr // a file that cannot be read is kept, never deleted blind
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			failed++
		}
		return nil
	})
	if failed > 0 {
		p.say("could not prune %d kept segment(s) past %s", failed, KeepUploadedKey)
	}
}

// KeepUploaded is how long shipped segments are kept, and why the setting could not be used
// ("" when it could).
func KeepUploaded() (time.Duration, string) { return keepUploaded() }
