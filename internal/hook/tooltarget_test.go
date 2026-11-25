//go:build unix

package hook

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xbew/livy/internal/tooltarget"
)

// Guards: a tool call's arguments, credentials or a denylisted name reaching the tool-target
// store, or the target reaching the ledger, which ships. Each row is a PostToolUse payload with
// planted canaries (fake keys, a password in a URL, a denylisted name); the target kept is the
// short label a trace needs and nothing else.
func TestToolTargetsKeepNoArgumentOrName(t *testing.T) {
	const deny = `{"version":1,"identities":[{"literal":"Zebulon Quux","role":"owner"}],"vendors":[]}`
	canaries := []string{"sk-ant-api03", "ghp_", "hunter2", "token=", "Zebulon", "Quux", "TestSecret", "/Users/", "deadbeef", "zq"}
	for _, tc := range []struct {
		name, tool, input, want string
		denylist                bool
	}{
		{"a shell command keeps its program", "Bash",
			`{"command":"cd /tmp && API_KEY=sk-ant-api03-FAKEFAKEFAKE curl -H 'X-Trace: ghp_FAKE' https://x.test"}`, "curl", true},
		{"git keeps its subcommand, not its options", "Bash",
			`{"command":"GIT_TOKEN=ghp_FAKE git -C /Users/someone/repo push origin HEAD:hunter2"}`, "git push", true},
		{"go test keeps no test name", "Bash", `{"command":"go test ./... -run TestSecret"}`, "go test", true},
		{"a fetch keeps the host only", "WebFetch",
			`{"url":"https://zq:hunter2@api.example.test:8443/v1/keys?token=abc","prompt":"Zebulon Quux"}`, "api.example.test", true},
		{"a read keeps the repo-relative path", "Read", `{"file_path":"REPO/internal/x.go"}`, "internal/x.go", true},
		{"a search keeps its folder, not its pattern", "Grep", `{"pattern":"hunter2","path":"REPO/internal"}`, "internal", true},
		{"a task label is scrubbed of a denylisted name", "Agent",
			`{"description":"Review Zebulon Quux's PR","prompt":"the brief: hunter2","subagent_type":"general-purpose"}`, "Review", true},
		{"an option's value is never taken for a subcommand", "Bash", `{"command":"kubectl --token deadbeef0123 get pods"}`, "kubectl", true},
		{"a wrapper with options keeps no value", "Bash", `{"command":"sudo -u zq make"}`, "sudo", true},
		{"env -S keeps no command line", "Bash", `{"command":"env -S 'wget --password hunter2 https://x.test'"}`, "env", true},
		{"credentials holding a '#' keep no host", "WebFetch", `{"url":"https://zq:hunter2#x@api.example.test/"}`, "", true},
		{"a numeric password holding a '#' keeps no host", "WebFetch", `{"url":"https://zq:1234#x@api.example.test/"}`, "", true},
		{"without a denylist nothing is kept", "Bash", `{"command":"curl https://x.test"}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := orgRepo(t)
			repo, _ := os.Getwd()
			if tc.denylist {
				if err := os.MkdirAll(data, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(data, "denylist.json"), []byte(deny), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			payload := `{"hook_event_name":"PostToolUse","session_id":"s-1","tool_name":"` + tc.tool + `","tool_use_id":"toolu_01AbC",` +
				`"duration_ms":5,"tool_input":` + strings.ReplaceAll(tc.input, "REPO", repo) + `,"tool_response":{"stdout":"hunter2"}}`
			Run([]string{"tool.call"}, stdinWith(t, payload), io.Discard)

			got, _ := tooltarget.Read(data, map[string]bool{"s-1": true})
			target := got["toolu_01AbC"]
			if tc.want == "" && target != "" || !strings.HasPrefix(target, tc.want) {
				t.Errorf("kept target %q, want %q", target, tc.want)
			}
			var kept []byte
			files, _ := filepath.Glob(filepath.Join(data, tooltarget.Dir, "*"))
			for _, f := range files {
				b, _ := os.ReadFile(f)
				kept = append(kept, b...)
			}
			if (tc.want != "") != (len(kept) > 0) {
				t.Errorf("store holds %d bytes in %v, want a line only when a target is kept", len(kept), files)
			}
			ledger, _ := os.ReadFile(filepath.Join(data, "ledger.jsonl"))
			for _, c := range canaries {
				if strings.Contains(target, c) || strings.Contains(string(kept), c) {
					t.Errorf("the target store holds %q: %s", c, kept)
				}
			}
			if strings.Contains(string(ledger), `"target"`) || (target != "" && strings.Contains(string(ledger), target)) {
				t.Errorf("the ledger carries the target: %s", ledger)
			}
			var line map[string]any
			if err := json.Unmarshal([]byte(strings.SplitN(string(ledger), "\n", 2)[0]), &line); err != nil || line["event"] != "tool.call" {
				t.Errorf("no tool.call line was written: %s", ledger)
			}
		})
	}
}
