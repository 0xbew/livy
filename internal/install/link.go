package install

import (
	"os"
	"path/filepath"
	"strings"
)

// linkName is the command an engineer types.
const linkName = "livy"

// linkDir is where the command is linked: ~/.local/bin, or /usr/local/bin when that is
// already on PATH and writable (it belongs to whatever else manages it, so it is not
// created or chosen otherwise). LIVY_LINK_DIR overrides, for a machine that keeps its own.
func linkDir(home string) string {
	if d := os.Getenv("LIVY_LINK_DIR"); d != "" {
		return d
	}
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p == "/usr/local/bin" && writable(p) {
			return p
		}
	}
	return filepath.Join(home, ".local", "bin")
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".livy-w")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

// isOurLink is true for a symlink that points at Livy's installed binary. A dangling link
// counts only when its target is in Livy's own layout (.livy/bin/livy): a stranger's tool
// of the same name is never repointed or removed.
func isOurLink(link, bin string) bool {
	target, err := os.Readlink(link)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	if filepath.Clean(target) == filepath.Clean(bin) {
		return true
	}
	if _, err := os.Stat(target); err == nil {
		return false
	}
	return strings.HasSuffix(filepath.ToSlash(target), "/.livy/bin/livy")
}

// installLink puts `livy` on the engineer's path. It returns the link's path when it is
// Livy's (so uninstall removes it), "" when it left something else alone.
func installLink(home, bin string, dry bool) (path, msg string, bad bool) {
	dir := linkDir(home)
	link := filepath.Join(dir, linkName)
	onPath := false
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		onPath = onPath || p == dir
	}
	hint := ""
	if !onPath {
		hint = " (not on PATH yet; add `export PATH=\"" + dir + ":$PATH\"` to your shell profile)"
	}
	fi, err := os.Lstat(link)
	switch {
	case err == nil && fi.Mode()&os.ModeSymlink != 0 && isOurLink(link, bin):
		if t, _ := os.Readlink(link); filepath.Clean(t) == filepath.Clean(bin) {
			return link, "command: " + link + " already points at " + bin + hint, false
		}
	case err == nil:
		return "", "command: " + link + " exists and is not Livy's; left alone", false
	}
	if dry {
		return "", "command: would link " + link + " to " + bin, false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "command: could not create " + dir + ": " + err.Error(), true
	}
	_ = os.Remove(link) // ours, or absent
	if err := os.Symlink(bin, link); err != nil {
		return "", "command: could not link " + link + ": " + err.Error(), true
	}
	return link, "command: " + link + " -> " + bin + hint, false
}

// removeLink removes the command link install made, only while it is still Livy's.
func removeLink(link, bin string, dry bool) note {
	if link == "" {
		return note{}
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 || !isOurLink(link, bin) {
		return note{}
	}
	if dry {
		return note{text: "would remove " + link}
	}
	if err := os.Remove(link); err != nil {
		return note{text: "could not remove " + link + ": " + err.Error(), failed: true}
	}
	return note{text: "removed " + link}
}
