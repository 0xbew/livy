package emit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/0xbew/scrub"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/prompts"
)

// The prompt stream is emit's second stream: the scrubbed text records `livy prompts` queues
// in prompt-text.jsonl, rotated into prompt-segments/<handle>/ and shipped to the prompt
// sink under source=prompt-text, with the ledger stream's rules: the key is the hash of the
// segment's bytes, the handle is frozen at rotation, writes are create-only, nothing is
// deleted before the sink confirms, and a failed upload keeps the segment. Every record is
// audited again just before it leaves, with the current denylist: one that fails is held in
// prompt-rejected/ and the rest of its segment still ships. Turned off, nothing ships.
const (
	// PromptSource is the prompt stream's object prefix.
	PromptSource = "source=prompt-text"
	// LastTextUpload is what doctor reads to tell a quiet prompt stream from a broken one.
	LastTextUpload = "last-text-upload.json"
	// maxPromptSegments bounds the work under the emit lock: the rest waits for the next sweep.
	maxPromptSegments = 20
)

// promptPass ships what is waiting. It runs inside the emit lock, after the ledger stream,
// and never stops it: a problem is reported and the text stays queued.
func promptPass(ctx context.Context, dir string, stderr io.Writer, dry bool) {
	defer func() {
		if p := recover(); p != nil {
			report(stderr, fmt.Sprintf("prompt text sweep raised %T", p))
		}
	}()
	if !prompts.UploadOn() {
		return // turned off: what was queued while it was on stays on this machine
	}
	live := filepath.Join(dir, prompts.TextFile)
	segs := filepath.Join(dir, prompts.SegmentsDir)
	if !dry {
		rotatePrompts(dir, live, segs, stderr)
	}
	pending := promptSegments(segs)
	if len(pending) == 0 {
		return
	}
	names, why := prompts.LoadDenylist()
	if names == nil {
		report(stderr, "prompt text kept, not sent: no usable denylist ("+why+")")
		return
	}
	sink, err := PromptSinkFromSettings(hook.UploadSettings())
	if err != nil {
		report(stderr, "prompt text kept, not sent: "+err.Error())
		return
	}
	if len(pending) > maxPromptSegments {
		pending = pending[:maxPromptSegments]
	}
	for _, seg := range pending {
		if dry {
			fmt.Fprintf(stderr, "emit: dry-run, would upload %s\n", sink.URL(PromptSource+"/…"))
			continue
		}
		if !holdHits(seg, filepath.Join(dir, prompts.RejectedDir), names, stderr) {
			continue
		}
		if !shipPrompts(ctx, dir, seg, sink, stderr) {
			return
		}
	}
}

func rotatePrompts(dir, live, segs string, stderr io.Writer) {
	if st, err := os.Stat(live); err != nil || st.Size() == 0 {
		return
	}
	owned := filepath.Join(segs, handleDir(Handle()))
	if err := os.MkdirAll(owned, 0o700); err != nil {
		report(stderr, "could not rotate the prompt text: "+err.Error())
		return
	}
	acquired, err := ledger.Lock(filepath.Join(dir, prompts.TextLock), rotateLockWait)
	if err != nil {
		report(stderr, "prompt text lock not acquired; not rotating, the next run will")
		return
	}
	defer acquired()
	if st, err := os.Stat(live); err == nil && st.Size() > 0 {
		if err := os.Rename(live, freeName(filepath.Join(owned, segmentName(clock.Now(), "")))); err != nil {
			report(stderr, "could not rotate the prompt text: "+err.Error())
		}
	}
}

func promptSegments(segs string) []string {
	var out []string
	_ = filepath.WalkDir(segs, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
			out = append(out, path)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return filepath.Base(out[i]) < filepath.Base(out[j]) })
	return out
}

// holdHits audits each record of a segment, moves every one that fails to prompt-rejected/ and
// rewrites the segment without it. It returns whether anything is left to ship.
func holdHits(seg, rejected string, names *scrub.Denylist, stderr io.Writer) bool {
	raw, err := os.ReadFile(seg)
	if err != nil {
		report(stderr, "cannot read a prompt text segment: "+err.Error())
		return false
	}
	var lines, keep []string
	stuck := false
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	for _, l := range lines {
		hits := scrub.AuditBody([]byte(l), names)
		if len(hits) == 0 {
			keep = append(keep, l)
			continue
		}
		id := "unreadable"
		var rec map[string]any
		if json.Unmarshal([]byte(l), &rec) == nil {
			id = fmt.Sprint(rec["prompt_id"])
		}
		sum := sha256.Sum256([]byte(l))
		held := filepath.Join(rejected, "held-"+hex.EncodeToString(sum[:])[:16]+".json")
		if err := os.MkdirAll(rejected, 0o700); err != nil {
			stuck = true // a refused record that cannot be set aside holds its segment back: never sent, never dropped
			report(stderr, "could not hold a refused prompt text record; the segment stays queued: "+err.Error())
			continue
		}
		if _, serr := os.Stat(held); serr != nil {
			if werr := os.WriteFile(held, []byte(l+"\n"), 0o600); werr != nil {
				stuck = true
				report(stderr, "could not hold a refused prompt text record; the segment stays queued: "+werr.Error())
				continue
			}
		}
		report(stderr, fmt.Sprintf("prompt text record %s NOT sent: the pre-send audit found %s; held in %s/",
			id, strings.Join(hits, ", "), prompts.RejectedDir))
	}
	if stuck {
		return false
	}
	switch {
	case len(keep) == len(lines):
		return len(keep) > 0
	case len(keep) == 0:
		_ = os.Remove(seg)
		return false
	}
	tmp := seg + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(keep, "\n")+"\n"), 0o600); err != nil {
		report(stderr, "could not rewrite a prompt text segment: "+err.Error())
		return false
	}
	if err := os.Rename(tmp, seg); err != nil {
		_ = os.Remove(tmp)
		report(stderr, "could not rewrite a prompt text segment; it stays queued: "+err.Error())
		return false
	}
	return true
}

// shipPrompts uploads one segment and deletes it once the sink has it. False means the sink
// refused it and the rest of this sweep should wait.
func shipPrompts(ctx context.Context, dir, seg string, sink Sink, stderr io.Writer) bool {
	raw, err := os.ReadFile(seg)
	if err != nil {
		report(stderr, "cannot read a prompt text segment: "+err.Error())
		return false
	}
	handle := ""
	if parent := filepath.Dir(seg); parent != filepath.Join(dir, prompts.SegmentsDir) && filepath.Base(parent) != anonymous {
		handle = filepath.Base(parent)
	}
	key, _, err := keyFor(PromptSource, raw, stampTime(seg), handle)
	if err != nil {
		report(stderr, err.Error())
		return false
	}
	body, err := Gzip(raw)
	if err != nil {
		report(stderr, "could not gzip a prompt text segment: "+err.Error())
		return false
	}
	putCtx, cancel := context.WithTimeout(ctx, putTimeout)
	err = sink.Put(putCtx, key, body)
	cancel()
	switch {
	case errors.Is(err, fs.ErrExist):
		fmt.Fprintf(stderr, "emit: already there, counted as shipped: %s\n", sink.URL(key))
	case err != nil:
		report(stderr, "prompt text not uploaded, kept for the next run: "+err.Error())
		return false
	default:
		fmt.Fprintf(stderr, "emit: shipped %s\n", sink.URL(key))
	}
	b, _ := json.Marshal(LastUpload{TS: clock.Seconds(clock.Now()), Object: sink.URL(key)})
	path := filepath.Join(dir, LastTextUpload)
	if err := writeLastTextUpload(path, b); err != nil {
		report(stderr, "shipped a prompt text segment but could not record it in "+LastTextUpload+": "+err.Error())
	}
	if err := os.Remove(seg); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "emit: shipped a prompt text segment but could not delete it (%v); the next run's upload is a no-op\n", err)
	}
	return true
}

// writeLastTextUpload replaces path with b through a temp file it removes on failure.
func writeLastTextUpload(path string, b []byte) error {
	if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
		_ = os.Remove(path + ".tmp")
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		_ = os.Remove(path + ".tmp")
		return err
	}
	return nil
}
