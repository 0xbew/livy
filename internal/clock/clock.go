// Package clock is the one place Livy reads the wall clock. LIVY_CONFORMANCE_NOW
// (epoch seconds) pins it, so the conformance harness can run Go and Python at one instant.
package clock

import (
	"math"
	"os"
	"strconv"
	"time"

	"github.com/0xbew/livy/internal/oldnames"
)

// Now is the current UTC time, or the pinned instant.
func Now() time.Time {
	v := os.Getenv("LIVY_CONFORMANCE_NOW")
	if v == "" {
		v = oldnames.Getenv(oldnames.LivyEnv("CONFORMANCE_NOW")) // test seam only; no warning
	}
	if v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			sec, frac := math.Modf(f)
			return time.Unix(int64(sec), int64(frac*1e9)).UTC()
		}
	}
	return time.Now().UTC()
}

// Seconds is datetime.isoformat(timespec="seconds") of an aware UTC time: 2026-11-03T12:00:00+00:00.
func Seconds(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05") + "+00:00" }
