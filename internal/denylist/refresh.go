package denylist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/0xbew/scrub"

	"github.com/0xbew/livy/internal/config"
)

// Result is one refresh: whether it left the machine as it should be, the one line a person
// reads, and why the record could not be written (the caller writes a ledger.error).
type Result struct {
	OK        bool
	Line      string
	RecordErr error
}

// Record is denylist-refresh.json: the last attempt, never the list's text.
type Record struct {
	CheckedAt string  `json:"checked_at"`
	Outcome   string  `json:"outcome"`
	Version   *int    `json:"version"`
	Reason    *string `json:"reason"`
	Source    string  `json:"source"`
	// The last SUCCESSFUL refresh, carried through failed attempts: when it ran, which source
	// it read (sourceID) and the hash of the copy it left. Doctor judges freshness from these,
	// not from the copy's mtime, which a touch or a copy changes.
	LastOKAt     string `json:"last_ok_at,omitempty"`
	LastOKSource string `json:"last_ok_source,omitempty"`
	LastOKSHA256 string `json:"last_ok_sha256,omitempty"`
}

// local is the copy's version (nil when there is none or it is unusable) and what a failed
// fetch leaves in place, in words.
func local(path string) (*int, string) {
	d, err := scrub.LoadDenylist(path)
	switch {
	case errors.Is(err, scrub.ErrDenylistMissing):
		return nil, "no local copy"
	case err != nil:
		return nil, "the local copy, which is unusable"
	}
	v := d.Version
	return &v, fmt.Sprintf("kept v%d", v)
}

// Refresh fetches the configured source into the local copy. It never panics or returns an
// error: the outcome is in the line and the record, where doctor reads it, because the daily
// job's output goes nowhere. The copy changes only when a whole valid version replaces it.
func Refresh(ctx context.Context, res config.Resolved, dataDir string, now time.Time) Result {
	path := Path(res, dataDir)
	shown := home(path)
	had, kept := local(path)
	unscrubbed := ""
	if had == nil {
		unscrubbed = "; names are NOT scrubbed"
	}
	src, err := FromSettings(res)
	if err == nil && src == nil {
		return Result{OK: true, Line: "no denylist.source configured: " + kept + unscrubbed}
	}
	var got fetched
	if err != nil {
		got = failed("%v", err)
	} else {
		got = fetch(ctx, src)
	}
	if got.outcome == Fetched {
		v, err := install(path, got.copy)
		var refused *scrub.DenylistError
		switch {
		case errors.As(err, &refused): // its message never quotes an entry
			got = fetched{outcome: Refused, reason: "the source is not a valid denylist: " + refused.Error()}
		case err != nil:
			got = failed("could not write the copy (%s)", pathless(err))
		default:
			got.version = v
		}
	}
	rec := Record{CheckedAt: now.UTC().Format(time.RFC3339), Outcome: got.outcome, Source: srcType(src)}
	if prev, ok := ReadRecord(dataDir); ok {
		rec.LastOKAt, rec.LastOKSource, rec.LastOKSHA256 = prev.LastOKAt, prev.LastOKSource, prev.LastOKSHA256
	}
	switch {
	case got.outcome == Fetched:
		rec.Version = &got.version
		rec.LastOKAt, rec.LastOKSource, rec.LastOKSHA256 = rec.CheckedAt, sourceID(src), copyHash(path)
	default:
		rec.Version = had
	}
	if got.reason != "" {
		r := home(got.reason)
		rec.Reason = &r
	}
	out := Result{RecordErr: writeRecord(filepath.Join(dataDir, StateName), rec)}
	switch got.outcome {
	case Fetched:
		out.OK, out.Line = true, fmt.Sprintf("v%d -> %s", got.version, shown)
	case NoVersion:
		out.OK, out.Line = true, "no version of the secret yet: "+kept+unscrubbed
	case Refused:
		out.Line = "REFUSED " + *rec.Reason + "; " + kept + unscrubbed
	default:
		out.Line = "NOT refreshed: " + *rec.Reason + "; " + kept + unscrubbed
	}
	return out
}

func srcType(s *Source) string {
	if s == nil {
		return ""
	}
	return s.Type
}

// install writes b as the copy, atomically and 0600 from the first byte (it names people),
// only when the reader accepts it whole: the temporary file is read with LoadDenylist, the
// very reader every sender uses, before it replaces anything; its refusal is a
// *scrub.DenylistError.
func install(path string, b []byte) (int, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	// A unique name: a refresh from install and one from the daily flush may overlap, and a
	// shared name would let one rename the other's half-written file into place.
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp") // mode 0600
	if err != nil {
		return 0, err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once renamed
	_, werr := f.Write(b)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o600)
	}
	if werr != nil {
		return 0, werr
	}
	d, err := scrub.LoadDenylist(tmp)
	if err != nil {
		return 0, err
	}
	return d.Version, os.Rename(tmp, path)
}

func writeRecord(path string, rec Record) error {
	b, _ := json.Marshal(rec)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // a no-op once renamed
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// ReadRecord is the last refresh's record; ok is false when there is none or it is unreadable.
func ReadRecord(dataDir string) (Record, bool) {
	var r Record
	b, err := os.ReadFile(filepath.Join(dataDir, StateName))
	if err != nil || json.Unmarshal(b, &r) != nil {
		return Record{}, false
	}
	return r, true
}

// email is an address-shaped run: gcloud's errors name the signed-in account.
var email = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)

// home shows the home folder as ~ and any address as [account]: both name a person.
func home(s string) string {
	if h, err := os.UserHomeDir(); err == nil && len(h) > 1 {
		s = strings.ReplaceAll(s, h, "~")
	}
	return email.ReplaceAllString(s, "[account]")
}

// sourceID names which source a refresh confirmed against (its type and where it points, never
// a credential), so a changed source is not read as confirmed by the old one.
func sourceID(s *Source) string {
	if s == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{s.Type, s.Path, s.Bucket, s.Object, s.Secret, s.Project}, "\x00")))
	return s.Type + ":" + hex.EncodeToString(sum[:])[:10]
}

// copyHash is the hash of the copy's bytes, "" when it cannot be read.
func copyHash(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
