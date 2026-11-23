package prompts

import (
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0xbew/scrub"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/extract/claudecode"
	"github.com/0xbew/livy/internal/extract/codex"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/ledger"
)

// extractLock is the extractor's own lock: a hand-run pass and a hook's pass must never write
// the same prompts at once.
const extractLock = "extract.lock"

// Main is `livy prompts` (also `prompt-capture`):
//
//	--upload on|off        the engineer's opt-in (off by default), kept in the user config
//	--backfill-days N      with upload on: send the last N days' prompts not yet sent, once
//	--dry-run              counts only; write nothing
//	--selftest
//
// roots are the Claude Code transcript roots. It returns the exit code.
func Main(args []string, roots []string, stdout, stderr io.Writer) int {
	a, code := parseArgs(args, stderr)
	if code != 0 {
		return code
	}
	upload, backfill, dry, selftest := a.upload, a.backfill, a.dry, a.selftest
	if selftest {
		return runSelftest(stderr, stdout)
	}
	if upload != "" && (backfill != nil || dry) {
		fmt.Fprintln(stderr, "prompt_capture: --upload changes the setting only; run the backfill or dry run separately")
		return 2
	}
	if upload != "" {
		if err := SetUpload(upload == "on"); err != nil {
			fmt.Fprintf(stderr, "prompt_capture: %v; prompt upload NOT changed\n", err)
			return 1
		}
		fmt.Fprintf(stderr, "prompt upload is %s\n", upload)
		return 0
	}
	dir := hook.DataDir()
	release, err := ledger.Lock(filepath.Join(dir, extractLock), 60*time.Second)
	if err != nil {
		fmt.Fprintln(stderr, "prompt_capture: an extraction is running; try again when it ends")
		return 1
	}
	defer release()
	counts := Run(Options{Roots: roots, CodexRoot: codex.Root(), DryRun: dry, BackfillDays: backfill,
		Now: clock.Now(), Dir: dir, Stderr: stderr})
	fmt.Fprintln(stdout, dumps(counts, true))
	return 0
}

// promptArgs are Main's flags.
type promptArgs struct {
	upload        string
	backfill      *int
	dry, selftest bool
}

// parseArgs reads Main's flags; a non-zero code is the exit code for a bad one.
func parseArgs(args []string, stderr io.Writer) (promptArgs, int) {
	var a promptArgs
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(args[i], "=")
		next := func() (string, bool) {
			if hasVal {
				return val, true
			}
			if i+1 >= len(args) {
				return "", false
			}
			i++
			return args[i], true
		}
		switch name {
		case "--dry-run":
			a.dry = true
		case "--selftest":
			a.selftest = true
		case "--upload":
			v, ok := next()
			if !ok || (v != "on" && v != "off") {
				fmt.Fprintln(stderr, "prompt_capture: --upload takes on or off")
				return a, 2
			}
			a.upload = v
		case "--backfill-days":
			v, ok := next()
			n, err := strconv.Atoi(v)
			if !ok || err != nil {
				fmt.Fprintln(stderr, "prompt_capture: --backfill-days takes a whole number")
				return a, 2
			}
			a.backfill = &n
		default:
			fmt.Fprintf(stderr, "prompt_capture: unknown argument %q\n", args[i])
			return a, 2
		}
	}
	return a, 0
}

// runSelftest proves a planted identity is refused by the audit and never reaches a text
// record, and that a prompt with upload off writes no text. Offline.
func runSelftest(stderr, stdout io.Writer) int {
	names, err := scrub.ParseDenylist([]byte(`{"identities":[{"literal":"Zebulon Quux","role":"owner"}],"vendors":[]}`), 1)
	if err != nil {
		fmt.Fprintln(stderr, "prompt_capture selftest: FAIL, the planted denylist was refused")
		return 1
	}
	p := claudecode.Prompt{ID: "prompt:selftest", TS: "2026-01-01T00:00:00Z", Repo: "example/selftest", Branch: "b",
		SessionID: "s", Text: "ask zebulon quux"}
	rec := textRecord(p, names)
	if strings.Contains(strings.ToLower(rec.json()), "quux") || len(scrub.AuditValue(rec.fields(), names)) > 0 {
		fmt.Fprintln(stderr, "prompt_capture selftest: FAIL, a planted identity survived the scrub")
		return 1
	}
	pass := &pass{counts: map[string]any{}, status: map[string]int{}, rules: map[string]int{}}
	events, records, _, _, _, _ := pass.decide([]claudecode.Prompt{p}, false, nil)
	if len(records) > 0 || len(events) != 1 || events[0]["text_status"] != "upload_off" {
		fmt.Fprintln(stderr, "prompt_capture selftest: FAIL, upload off still produced text")
		return 1
	}
	fmt.Fprintln(stdout, "prompt_capture selftest: ok")
	return 0
}
