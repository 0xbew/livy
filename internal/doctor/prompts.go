package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/emit"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/prompts"
)

// checkPrompts is whether prompt upload is on and working. Off with nothing queued is ok; off
// with records left from when it was on is said; on with no usable denylist or no prompt sink
// fails, since nothing can leave; text waiting longer than StaleAfter fails, since it is not
// reaching the store.
func checkPrompts(r *Report, dir string) {
	count, oldest := promptQueue(dir)
	rejected := 0
	if entries, err := os.ReadDir(filepath.Join(dir, prompts.RejectedDir)); err == nil {
		rejected = len(entries)
	}
	if !prompts.UploadOn() {
		if count > 0 {
			r.add(Warn, "prompt upload", "off; %d text record(s) queued while it was on stay on this machine", count)
			return
		}
		r.add(OK, "prompt upload", "off (livy prompts --upload on to opt in)")
		return
	}
	if ok, why := prompts.Denylist(); !ok {
		r.add(Fail, "prompt upload", "on, but no usable denylist (%s): no text can be written", why)
		return
	}
	if _, err := emit.PromptSinkFromSettings(hook.Settings()); err != nil {
		r.add(Fail, "prompt upload", "on, but %v: queued text cannot leave", err)
		return
	}
	when := "never"
	if raw, err := os.ReadFile(filepath.Join(dir, emit.LastTextUpload)); err == nil {
		var last emit.LastUpload
		if json.Unmarshal(raw, &last) == nil && last.TS != "" {
			when = last.TS
		}
	}
	detail := fmt.Sprintf("on; last text record uploaded %s; %d queued", when, count)
	if rejected > 0 {
		detail += fmt.Sprintf("; %d record(s) held by the pre-send audit (%s/)", rejected, prompts.RejectedDir)
	}
	stale := oldest > StaleAfter
	if stale {
		detail += " - STALE, text is not reaching the prompt store"
	}
	switch {
	case stale:
		r.add(Fail, "prompt upload", "%s", detail)
	case rejected > 0:
		r.add(Warn, "prompt upload", "%s", detail)
	default:
		r.add(OK, "prompt upload", "%s", detail)
	}
}

// promptQueue is the records waiting on this machine and the age of the oldest file holding
// them. Read-only.
func promptQueue(dir string) (count int, oldest time.Duration) {
	paths, _ := filepath.Glob(filepath.Join(dir, prompts.SegmentsDir, "*", "*.jsonl"))
	flat, _ := filepath.Glob(filepath.Join(dir, prompts.SegmentsDir, "*.jsonl"))
	for _, p := range append(append(paths, flat...), filepath.Join(dir, prompts.TextFile)) {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(l) != "" {
				count++
			}
		}
		if st, err := os.Stat(p); err == nil {
			oldest = max(oldest, clock.Now().Sub(st.ModTime()))
		}
	}
	return count, oldest
}
