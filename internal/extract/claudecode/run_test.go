//go:build unix

package claudecode

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setup makes an in-scope repo (origin under acme), a projects root, and a data dir.
func setup(t *testing.T) (repo, root, data string) {
	t.Helper()
	tmp := t.TempDir()
	repo = filepath.Join(tmp, "repo")
	for _, args := range [][]string{{"init", "-q", "-b", "main", repo},
		{"-C", repo, "remote", "add", "origin", "https://github.com/acme/app.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Setenv("HOME", tmp)
	t.Setenv("LIVY_SCOPE_ORGS", "acme")
	return repo, filepath.Join(tmp, "projects"), filepath.Join(tmp, "data")
}

// assistant is one synthetic assistant record of API message id with an output count.
func assistant(repo, id string, output int, ts string) map[string]any {
	return map[string]any{"type": "assistant", "cwd": repo, "gitBranch": "main", "sessionId": "s1",
		"version": "2.1.0", "timestamp": ts, "message": map[string]any{"id": id, "model": "m",
			"content": []any{map[string]any{"type": "text", "text": "x"}},
			"usage":   map[string]any{"input_tokens": 1, "output_tokens": output}}}
}

func writeLines(t *testing.T, path string, recs ...map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, r := range recs {
		b, _ := json.Marshal(r)
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func ledgerLines(t *testing.T, data string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(data, "ledger.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("ledger line is not JSON: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func run(t *testing.T, root, data string, now time.Time) {
	t.Helper()
	Run(Options{Roots: []string{root}, Dir: data, Stderr: io.Discard, Now: now})
}

// TestFormatDriftHoldsTheFileAndSaysSoOnce guards contract rule 6: a record the reader does
// not recognise must write a ledger.error naming the Claude Code version and stop that file
// before it, never pass silently (the Python reader counted such calls as lost and read on) and never
// repeat the error on every run while the file stays held.
func TestFormatDriftHoldsTheFileAndSaysSoOnce(t *testing.T) {
	for name, bad := range map[string]map[string]any{
		"assistant record without usage": {"type": "assistant", "cwd": "", "version": "9.9.9",
			"timestamp": "2026-11-01T10:00:01.000Z", "message": map[string]any{"id": "m-bad", "usage_v2": map[string]any{}}},
		"unknown record type": {"type": "brand_new_kind", "version": "9.9.9", "timestamp": "2026-11-01T10:00:01.000Z"},
		"user record without a message": {"type": "user", "version": "9.9.9", "timestamp": "2026-11-01T10:00:01.000Z",
			"message_v2": map[string]any{"content": "x"}},
		"user block of an unknown type": {"type": "user", "version": "9.9.9", "timestamp": "2026-11-01T10:00:01.000Z",
			"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "prompt_v2", "body": "x"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			repo, root, data := setup(t)
			bad["cwd"] = repo
			path := filepath.Join(root, "-p", "s1.jsonl")
			writeLines(t, path, assistant(repo, "m-1", 3, "2026-11-01T10:00:00.000Z"),
				assistant(repo, "m-2", 3, "2026-11-01T10:00:00.500Z"), bad,
				assistant(repo, "m-after", 4, "2026-11-01T10:00:02.000Z"))
			old := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
			run(t, root, data, old)
			run(t, root, data, old)
			var calls, errs []string
			for _, l := range ledgerLines(t, data) {
				switch l["event"] {
				case "usage.api_call":
					calls = append(calls, l["message_id"].(string))
				case "ledger.error":
					errs = append(errs, l["reason"].(string))
				}
			}
			// m-2 is the last message to start before the record, so it may still have records
			// after it: it is held with the rest of the file.
			if strings.Join(calls, ",") != "m-1" {
				t.Errorf("usage lines for %v, want only m-1", calls)
			}
			if len(errs) != 1 || !strings.Contains(errs[0], "Claude Code 9.9.9") {
				t.Errorf("errors %q, want one naming Claude Code 9.9.9 across two runs", errs)
			}
		})
	}
}

// TestStreamingMessageIsWrittenOnceWithItsFinalCount guards contract rule 6: a message still
// streaming in a live file (output 5, later 201) must not be written until its final record,
// and never twice across runs.
func TestStreamingMessageIsWrittenOnceWithItsFinalCount(t *testing.T) {
	repo, root, data := setup(t)
	path := filepath.Join(root, "-p", "s1.jsonl")
	writeLines(t, path, assistant(repo, "m-done", 7, "2026-11-01T10:00:00.000Z"),
		assistant(repo, "m-live", 5, "2026-11-01T10:00:01.000Z"))
	written := time.Date(2026, 11, 1, 10, 0, 2, 0, time.UTC)
	if err := os.Chtimes(path, written, written); err != nil {
		t.Fatal(err)
	}
	run(t, root, data, written.Add(time.Minute)) // modified a minute ago: still live
	writeLines(t, path, assistant(repo, "m-live", 201, "2026-11-01T10:00:03.000Z"))
	if err := os.Chtimes(path, written, written); err != nil {
		t.Fatal(err)
	}
	run(t, root, data, written.Add(time.Hour)) // settled
	got := map[string][]float64{}
	for _, l := range ledgerLines(t, data) {
		if l["event"] == "usage.api_call" {
			id := l["message_id"].(string)
			got[id] = append(got[id], l["output_tokens"].(float64))
		}
	}
	if len(got) != 2 || len(got["m-done"]) != 1 || len(got["m-live"]) != 1 || got["m-live"][0] != 201 {
		t.Errorf("usage output counts by message %v, want m-done once and m-live once at 201", got)
	}
}

// TestCallIsNotWrittenAgainAfterACutShortPass guards contract rule 6 (never twice across
// runs): a pass killed after appending lines but before saving its offsets, and a call copied
// into a second transcript and read in a later run, must not write the same call again.
func TestCallIsNotWrittenAgainAfterACutShortPass(t *testing.T) {
	repo, root, data := setup(t)
	// Runs an hour after the files' clock, so every file is settled.
	old := time.Now().Add(time.Hour)
	writeLines(t, filepath.Join(root, "-p", "s1.jsonl"), assistant(repo, "m-1", 3, "2026-11-01T10:00:00.000Z"))
	run(t, root, data, old)
	// The pass is cut short: its offsets never reach the state file.
	if err := os.Remove(filepath.Join(data, stateName)); err != nil {
		t.Fatal(err)
	}
	run(t, root, data, old)
	// A later session's transcript carries a copy of the same call.
	writeLines(t, filepath.Join(root, "-p", "s2.jsonl"), assistant(repo, "m-1", 3, "2026-11-01T10:00:00.000Z"))
	run(t, root, data, old)
	n := 0
	for _, l := range ledgerLines(t, data) {
		if l["event"] == "usage.api_call" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("m-1 written %d times across three runs, want once", n)
	}
}

// TestPRLinkIsNotWrittenAgainAfterACutShortPass guards contract rule 6 for work.pr_linked: a
// pass killed after appending a PR link but before saving state must not write it again.
func TestPRLinkIsNotWrittenAgainAfterACutShortPass(t *testing.T) {
	_, root, data := setup(t)
	when := time.Now().Add(time.Hour) // every file settled
	writeLines(t, filepath.Join(root, "-p", "s1.jsonl"), map[string]any{"type": "pr-link", "sessionId": "s1",
		"prNumber": 7, "prRepository": "acme/app", "timestamp": "2026-11-01T10:00:01.000Z"})
	run(t, root, data, when)
	if err := os.Remove(filepath.Join(data, stateName)); err != nil { // the pass is cut short
		t.Fatal(err)
	}
	run(t, root, data, when)
	n := 0
	for _, l := range ledgerLines(t, data) {
		if l["event"] == "work.pr_linked" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the PR link was written %d times across two runs, want once", n)
	}
}

// TestSkillUsedOutsideTheRepoIsRecorded guards the work Mac's "Skills loaded: 0" on every PR:
// Claude Code started outside the repo, so the Skill hook (scoped by its folder) never wrote, and
// an agent that read a SKILL.md with the Read tool was never counted. Each row is a subagent's
// call shaped as Claude Code writes it (tool_use, tool_result, and for the Skill tool the
// isMeta message naming the skill's folder); a skill used in no in-scope repo leaves nothing.
func TestSkillUsedOutsideTheRepoIsRecorded(t *testing.T) {
	type use struct{ skill, trigger, repo string }
	for name, tc := range map[string]struct {
		build func(repo, outside string) []map[string]any
		want  []use
		// grown is true when a pass runs first while the file may still grow, so the agent's next
		// call (the last message) is left to the next pass.
		grown bool
	}{
		"Skill tool from outside, the skill's folder in the repo": {func(repo, outside string) []map[string]any {
			return skillCall(outside, "toolu_1", "Skill", map[string]any{"skill": "deploy"},
				filepath.Join(repo, ".claude", "skills", "deploy"))
		}, []use{{"deploy", "agent", "acme/app"}}, false},
		"Read of the repo's SKILL.md from outside": {func(repo, outside string) []map[string]any {
			return skillCall(outside, "toolu_2", "Read",
				map[string]any{"file_path": filepath.Join(repo, ".claude", "skills", "review", "SKILL.md")}, "")
		}, []use{{"review", "read", "acme/app"}}, false},
		"a user skill from outside, placed by the agent's next call in the repo": {func(repo, outside string) []map[string]any {
			recs := skillCall(outside, "toolu_3", "Skill", map[string]any{"skill": "notes"},
				filepath.Join(outside, ".claude", "skills", "notes"))
			next := assistant(repo, "m-next", 2, "2026-11-01T10:00:09.000Z")
			next["agentId"], next["isSidechain"] = "a1", true
			return append(recs, next)
		}, []use{{"notes", "agent", "acme/app"}}, false},
		"a user skill from outside, the agent's next call read in a later pass": {func(repo, outside string) []map[string]any {
			recs := skillCall(outside, "toolu_6", "Skill", map[string]any{"skill": "notes"},
				filepath.Join(outside, ".claude", "skills", "notes"))
			next := assistant(repo, "m-next", 2, "2026-11-01T10:00:09.000Z")
			next["agentId"], next["isSidechain"] = "a1", true
			return append(recs, next)
		}, []use{{"notes", "agent", "acme/app"}}, true},
		"a user skill in a session with no in-scope call": {func(_, outside string) []map[string]any {
			return skillCall(outside, "toolu_4", "Skill", map[string]any{"skill": "notes"},
				filepath.Join(outside, ".claude", "skills", "notes"))
		}, nil, false},
		"a Skill call that failed": {func(repo, _ string) []map[string]any {
			recs := skillCall(repo, "toolu_5", "Skill", map[string]any{"skill": "nope"}, "")
			result := recs[1]["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
			result["is_error"], result["content"] = true, "<tool_use_error>Unknown skill: nope</tool_use_error>"
			return recs
		}, nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			repo, root, data := setup(t)
			outside := filepath.Join(filepath.Dir(repo), "outside")
			if err := os.MkdirAll(outside, 0o777); err != nil {
				t.Fatal(err)
			}
			writeLines(t, filepath.Join(root, "-p", "s1", "subagents", "agent-a1.jsonl"), tc.build(repo, outside)...)
			if tc.grown {
				run(t, root, data, time.Now())
			}
			run(t, root, data, time.Now().Add(time.Hour)) // the file has settled
			var got []use
			for _, l := range ledgerLines(t, data) {
				if l["event"] == "skill.loaded" {
					got = append(got, use{l["skill"].(string), l["trigger"].(string), l["repo"].(string)})
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("skill.loaded lines %v, want %v", got, tc.want)
			}
		})
	}
}

// skillCall is a subagent's tool call at cwd and its result, then, for a
// Skill call whose skill is in dir, the message Claude Code adds naming that folder.
func skillCall(cwd, id, tool string, input map[string]any, dir string) []map[string]any {
	call := assistant(cwd, "m-"+id, 3, "2026-11-01T10:00:01.000Z")
	call["agentId"], call["isSidechain"] = "a1", true
	call["message"].(map[string]any)["content"] = []any{map[string]any{"type": "tool_use", "id": id, "name": tool, "input": input}}
	result := map[string]any{"type": "tool_result", "tool_use_id": id, "content": "Launching skill"}
	user := func(ts string, content []any) map[string]any {
		return map[string]any{"type": "user", "cwd": cwd, "sessionId": "s1", "agentId": "a1", "isSidechain": true,
			"version": "2.1.0", "timestamp": ts, "message": map[string]any{"role": "user", "content": content}}
	}
	out := []map[string]any{call, user("2026-11-01T10:00:02.000Z", []any{result})}
	if dir != "" {
		meta := user("2026-11-01T10:00:02.000Z", []any{map[string]any{"type": "text",
			"text": "Base directory for this skill: " + dir + "\n\n# The skill's text"}})
		meta["isMeta"], meta["sourceToolUseID"] = true, id
		out = append(out, meta)
	}
	return out
}
