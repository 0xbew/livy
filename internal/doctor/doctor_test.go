package doctor

import (
	"testing"
	"time"
)

// Guards: the failure doctor exists for, a broken machine that looks quiet (rule 24): a
// refused writer, an upload that stopped, or data that never left; and the false alarm
// that trains people to ignore it, a quiet machine whose first new event reads as stale.
// Rows are hand-worked from the contract's cases.
func TestUploadVerdict(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	hour := time.Hour
	var never time.Time
	for _, c := range []struct {
		name                 string
		last, failed, oldest time.Time
		wantStatus, wantWhy  string
	}{
		{"fresh install, nothing yet", never, never, never, Warn, WhyNothing},
		{"first event a minute ago, never shipped yet", never, never, now.Add(-time.Minute), Warn, WhyPending},
		{"captured three days ago, never shipped", never, never, now.Add(-72 * hour), Fail, WhyNever},
		{"shipped an hour ago, new data since", now.Add(-hour), never, now.Add(-time.Minute), OK, ""},
		{"data waiting three days", now.Add(-80 * hour), never, now.Add(-72 * hour), Fail, WhyStale},
		{"quiet three days, then one event a minute ago", now.Add(-72 * hour), never, now.Add(-time.Minute), OK, ""},
		{"quiet for three days, nothing waiting", now.Add(-72 * hour), never, never, OK, ""},
		{"refused after the last upload", now.Add(-2 * hour), now.Add(-hour), now.Add(-90 * time.Minute), Fail, WhyFailed},
		{"refused once, shipped since", now.Add(-hour), now.Add(-2 * hour), never, OK, ""},
		{"refused, never shipped", never, now.Add(-hour), now.Add(-2 * hour), Fail, WhyFailed},
	} {
		if s, why := UploadVerdict(now, c.last, c.failed, c.oldest, !c.oldest.IsZero()); s != c.wantStatus || why != c.wantWhy {
			t.Errorf("%s: UploadVerdict = %s %q, want %s %q", c.name, s, why, c.wantStatus, c.wantWhy)
		}
	}
}
