package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MinClaudeVersion is the oldest Claude Code that records a model timeline; older ones
// capture the rest but no model switches.
var MinClaudeVersion = [3]int{2, 1, 251}

var versionRE = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

func versionOf(text string) (v [3]int, ok bool) {
	m := versionRE.FindStringSubmatch(text)
	if m == nil {
		return v, false
	}
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

func dotted(v [3]int) string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

func less(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// claudeBinaries is every `claude` this machine would run, by path, with what `--version`
// printed. Only the newest VS Code extension counts: a machine keeps every past one, and
// only the newest launches.
func claudeBinaries(ctx context.Context, home string) map[string]string {
	cands := []string{os.Getenv("CLAUDE_CODE_EXECPATH")}
	if p, err := exec.LookPath("claude"); err == nil {
		cands = append(cands, p)
	}
	cands = append(cands, "/opt/homebrew/bin/claude", "/usr/local/bin/claude", filepath.Join(home, ".claude", "local", "claude"))
	ext, _ := filepath.Glob(filepath.Join(home, ".vscode", "extensions", "anthropic.claude-code-*", "resources", "native-binary", "claude"))
	sort.Slice(ext, func(i, j int) bool {
		vi, _ := versionOf(filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(ext[i])))))
		vj, _ := versionOf(filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(ext[j])))))
		return less(vi, vj)
	})
	if len(ext) > 0 {
		cands = append(cands, ext[len(ext)-1])
	}
	found := map[string]string{}
	for _, p := range cands {
		if p == "" {
			continue
		}
		if _, dup := found[p]; dup {
			continue
		}
		if fi, err := os.Stat(p); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		out, err := exec.CommandContext(cctx, p, "--version").Output()
		cancel()
		if err == nil {
			found[p] = strings.TrimSpace(string(out))
		}
	}
	return found
}

func checkClaudeVersions(ctx context.Context, r *Report, home string) {
	bins := claudeBinaries(ctx, home)
	if len(bins) == 0 {
		r.add(Warn, "claude version", "no claude binary found to check")
		return
	}
	status, detail := claudeVersionRow(bins)
	r.add(status, "claude version", "%s", detail)
}

// claudeVersionRow judges what each claude binary's `--version` printed (keyed by path).
func claudeVersionRow(bins map[string]string) (status, detail string) {
	var old, unread []string
	seen := map[string]bool{}
	for p, text := range bins {
		v, ok := versionOf(text)
		if !ok {
			unread = append(unread, p)
			continue
		}
		if less(v, MinClaudeVersion) {
			where := p
			if strings.Contains(p, ".vscode/extensions") {
				where = "vscode extension"
			}
			s := fmt.Sprintf("%s (%s)", dotted(v), where)
			if !seen[s] {
				seen[s] = true
				old = append(old, s)
			}
		}
	}
	min := dotted(MinClaudeVersion)
	if len(unread) > 0 {
		sort.Strings(unread)
		note := fmt.Sprintf("cannot read the version of %s: its `--version` printed no version number, so it may be older than %s", strings.Join(unread, ", "), min)
		if len(old) > 0 {
			sort.Strings(old)
			note += "; older than " + min + ": " + strings.Join(old, ", ")
		}
		return Warn, note
	}
	if len(old) > 0 {
		sort.Strings(old)
		return Warn, fmt.Sprintf("%d in use; older than %s: %s — those record NO model timeline (update with: npm i -g @anthropic-ai/claude-code)",
			len(bins), min, strings.Join(old, ", "))
	}
	return OK, fmt.Sprintf("%d found, all at least %s", len(bins), min)
}
