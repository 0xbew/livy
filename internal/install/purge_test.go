package install

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Guards: `livy uninstall --purge` left captured prompt text, the OTEL stream and the relay
// and Jev keys behind, and called them "not Livy's". The expected names are typed from where
// each package writes them, not read from the purge list.
func TestPurgeDeletesEveryFileLivyKeeps(t *testing.T) {
	data := []string{"ledger.jsonl", "ledger.lock", "spill/", "segments/", "rejected/", "emit.lock",
		"emit-last-run", "last-upload.json", "spotcheck.pause.4242", "last-upload.json.tmp", "denylist.json", "denylist-refresh.json",
		".denylist.json.123.tmp", "prompt-text.jsonl", "prompt-segments/", "prompt-rejected/",
		"prompt-capture-state.json", "prompt-text.lock", "last-text-upload.json", "otel/",
		"codex-otel-relay.key", "codex-otel-relay.json", "jev.key", "spotcheck.pause", "extract.lock",
		"usage-backfill.lock", "extract-last-run", "extract-state.json", "extract-codex-state.json",
		"extract-written.log", "tool-backfill.lock", "tool-backfill-state.json", "dogfood-unshipped/"}
	user := []string{"config.json", "config.json.lock", "install.json", "dogfood.json", "doctor-status.json",
		"migrate/", "bin/"}
	stranger := []string{"notes.txt", "ledger.jsonl.bak"}
	t.Run("data folder is the user folder", func(t *testing.T) {
		dir := t.TempDir()
		lay(t, dir, slices.Concat(data, user, stranger))
		left, err := purge(dir, dir, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if got := remaining(t, dir); !slices.Equal(got, []string{"ledger.jsonl.bak", "notes.txt"}) {
			t.Errorf("purge left %v, want only the stranger files", got)
		}
		if len(left) != 2 {
			t.Errorf("purge reported %v as left, want the two stranger files", left)
		}
	})
	t.Run("LIVY_DIR elsewhere: the user folder is purged too", func(t *testing.T) {
		dataDir, userDir := t.TempDir(), t.TempDir()
		lay(t, dataDir, slices.Concat(data, stranger))
		lay(t, userDir, slices.Concat(user, []string{"ledger.jsonl"}))
		if _, err := purge(dataDir, userDir, "", false); err != nil {
			t.Fatal(err)
		}
		if got := remaining(t, dataDir); !slices.Equal(got, []string{"ledger.jsonl.bak", "notes.txt"}) {
			t.Errorf("data folder: purge left %v, want only the stranger files", got)
		}
		if _, err := os.Stat(userDir); !os.IsNotExist(err) {
			t.Errorf("user folder: purge left %v, want it gone", remaining(t, userDir))
		}
	})
	t.Run("the Python tool's folder is never purged, as data or user folder", func(t *testing.T) {
		for _, which := range []string{"data", "user"} {
			python, other := t.TempDir(), t.TempDir()
			lay(t, python, []string{"ledger.jsonl", "segments/"})
			dataDir, userDir := python, other
			if which == "user" {
				dataDir, userDir = other, python
			}
			if _, err := purge(dataDir, userDir, python, false); err != nil {
				t.Fatal(err)
			}
			if got := remaining(t, python); !slices.Equal(got, []string{"ledger.jsonl", "segments"}) {
				t.Errorf("Python folder as the %s folder: purge left %v, want [ledger.jsonl segments]", which, got)
			}
		}
	})
	t.Run("an unfinished uninstall keeps its record and binary", func(t *testing.T) {
		dir := t.TempDir()
		lay(t, dir, slices.Concat(data, user))
		if _, err := purge(dir, dir, "", true); err != nil {
			t.Fatal(err)
		}
		if got := remaining(t, dir); !slices.Equal(got, []string{"bin", "install.json"}) {
			t.Errorf("purge left %v, want [bin install.json]", got)
		}
	})
}

// fileNameConst is a constant that names a file or folder: its identifier ends in Name, File,
// Dir or Lock, or its value has a file extension Livy writes.
var (
	fileNameIdent = regexp.MustCompile(`(Name|File|Dir|Lock)$`)
	fileNameValue = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	fileNameExt   = regexp.MustCompile(`\.(jsonl|json|lock|key|log|pause)$`)
)

// notDataFiles are file-name constants that never sit in the data or user folder.
var notDataFiles = map[string]string{
	"livy":          "the command link, in ~/.local/bin; uninstall removes it by name",
	"file_path":     "a tool input field, not a file",
	"settings.json": "Claude Code's settings",
	"managed.json":  "the admin's managed file",
	"config.toml":   "Codex's config",
	"hooks.json":    "Codex's hooks file, in Codex's home",
	"post-checkout": "a git hook",
	"state.json":    "the Amp plugin's state, inside amp-plugin/, which purge deletes whole",
	"last-turn":     "the Amp plugin's last recorded turn, inside amp-plugin/, which purge deletes whole",
	"built.json":    "the keyspace index's manifest, inside index/, which purge deletes whole",
	"lock":          "the keyspace index's lock, inside index/, which purge deletes whole",
	"pending":       "the keyspace index's pending lines, inside index/, which purge deletes whole",
	"stale":         "the keyspace index's stale mark, inside index/, which purge deletes whole",
}

// Guards: a new file a package keeps in the data folder that purge does not know, which is
// how prompt text and keys were left behind. Every file-name constant in the tree must be
// deleted by purge, or be named in notDataFiles with the reason.
func TestPurgeKnowsEveryFileNameConstant(t *testing.T) {
	var names []string
	root := filepath.Join("..", "..", "internal")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, id := range spec.Names {
				if i >= len(spec.Values) {
					break
				}
				lit, ok := spec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, _ := strconv.Unquote(lit.Value)
				if fileNameValue.MatchString(v) && (fileNameIdent.MatchString(id.Name) || fileNameExt.MatchString(v)) {
					names = append(names, v)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	for _, n := range names {
		if _, skip := notDataFiles[n]; skip {
			continue
		}
		dir := t.TempDir()
		lay(t, dir, []string{n})
		if _, err := purge(dir, dir, "", false); err != nil {
			t.Fatal(err)
		}
		if got := remaining(t, dir); len(got) != 0 {
			t.Errorf("purge leaves %q; add it to its package's data names, or to notDataFiles with why", n)
		}
	}
}

// lay creates each name in dir: a folder holding one file when it ends in "/", else a file.
func lay(t *testing.T, dir string, names []string) {
	t.Helper()
	for _, n := range names {
		p := filepath.Join(dir, strings.TrimSuffix(n, "/"))
		if strings.HasSuffix(n, "/") {
			if err := os.MkdirAll(p, 0o700); err != nil {
				t.Fatal(err)
			}
			p = filepath.Join(p, "x.jsonl")
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// remaining lists dir's entries, sorted; none when dir is gone.
func remaining(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
