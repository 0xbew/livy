package denylist

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/0xbew/livy/internal/hook"
)

// Main is `livy install --refresh-denylist`, which the daily flush also runs: one refresh,
// one line. Exit 1 when the refresh failed or was refused (the copy is kept either way).
func Main(ctx context.Context, stdout, stderr io.Writer) int {
	r := run(ctx, stderr)
	fmt.Fprintf(stdout, "denylist         %s\n", r.Line)
	if !r.OK {
		fmt.Fprintf(stderr, "livy: denylist %s\n", r.Line)
		return 1
	}
	return 0
}

// InstallStep is install's denylist line, after the config that names the source. Its
// outcome never fails the install: without a copy capture runs and names are not
// scrubbed, which doctor reports.
func InstallStep(dry bool, stderr io.Writer) string {
	if dry {
		return "would refresh the copy from denylist.source, if one is configured"
	}
	r := run(context.Background(), stderr)
	if !r.OK {
		fmt.Fprintf(stderr, "livy: denylist %s\n", r.Line)
	}
	return r.Line
}

func run(ctx context.Context, stderr io.Writer) Result {
	r := Refresh(ctx, hook.Settings(), hook.DataDir(), time.Now())
	if r.RecordErr != nil {
		// Without the record doctor cannot say the last refresh failed: say it where it is read.
		hook.WriteError("denylist: could not record the refresh: "+pathless(r.RecordErr), stderr)
	}
	return r
}
