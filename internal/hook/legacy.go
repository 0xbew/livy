package hook

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/0xbew/livy/internal/config"
	"github.com/0xbew/livy/internal/oldnames"
)

// Livy had another name before (oldnames.Livy). Its settings keep answering to the old
// names so no install breaks; install writes only the new ones, and the first event of a
// session writes one ledger.error per old name still in use so the owner can see who has
// not moved. A build with no old name (the public one) reads none of them.

const newDirName = ".livy"

// userDir is the user's folder: ~/.livy, or the old one when only that exists. Livy never
// moves it.
func userDir(userHome string, exists func(string) bool) string {
	d, _ := chooseUserDir(userHome, exists)
	return d
}

func chooseUserDir(userHome string, exists func(string) bool) (string, []string) {
	cur := filepath.Join(userHome, newDirName)
	oldName := oldnames.LivyDir()
	if oldName == "" {
		return cur, nil
	}
	if old := filepath.Join(userHome, oldName); !exists(cur) && exists(old) {
		return old, []string{"~/" + oldName + " is in use because ~/.livy does not exist; Livy will not move it, rename it to ~/.livy when ready"}
	}
	return cur, nil
}

// oldDirEnvs are the old variables that name the ledger folder, in the order they are read
// after LIVY_DIR; a build with no old names has none.
func oldDirEnvs() []string {
	var out []string
	for _, n := range []string{oldnames.LivyEnv("DIR"), oldnames.RecorderEnv("DIR")} {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// resolveDataDir is where the ledger lives, and the notes about old names it relied on.
func resolveDataDir(getenv func(string) string, userHome string, exists func(string) bool) (string, []string) {
	if d := getenv("LIVY_DIR"); d != "" {
		return d, nil
	}
	if n := oldnames.LivyEnv("DIR"); n != "" && getenv(n) != "" {
		return getenv(n), []string{n + " is set; rename it to LIVY_DIR"}
	}
	if n := oldnames.RecorderEnv("DIR"); n != "" && getenv(n) != "" {
		return getenv(n), nil
	}
	return chooseUserDir(userHome, exists)
}

// chooseManaged is the admin's managed file: the Livy path, or the old name's path while only
// that file exists.
func chooseManaged(goos, programData string, exists func(string) bool) (string, []string) {
	cur, old := managedPaths(goos, programData)
	if old != "" && !exists(cur) && exists(old) {
		return old, []string{"the managed file is read from the old " + oldnames.Livy + " path " + old + "; move it to " + cur}
	}
	return cur, nil
}

// managedPaths is the managed file's path on goos, and its path under the old name ("" with none).
func managedPaths(goos, programData string) (cur, old string) {
	name := oldnames.Livy
	switch goos {
	case "darwin":
		cur, old = "/Library/Application Support/Livy/managed.json", filepath.Join("/Library/Application Support", name, "managed.json")
	case "windows":
		cur, old = filepath.Join(programData, "Livy", "managed.json"), filepath.Join(programData, name, "managed.json")
	default:
		cur, old = "/etc/livy/managed.json", filepath.Join("/etc", strings.ToLower(name), "managed.json")
	}
	if name == "" {
		old = ""
	}
	return cur, old
}

// oldNameNotes is every old name this machine still depends on, in a stable order.
func oldNameNotes() []string {
	var notes []string
	_, n := resolveDataDir(os.Getenv, home(), pathExists)
	notes = append(notes, n...)
	if os.Getenv("LIVY_DIR") != "" || slices.ContainsFunc(oldDirEnvs(), func(n string) bool { return os.Getenv(n) != "" }) {
		// the ledger folder is overridden, but the user config folder still resolves from home
		_, n = chooseUserDir(home(), pathExists)
		notes = append(notes, n...)
	}
	_, n = chooseManaged(runtime.GOOS, os.Getenv("ProgramData"), mayExist)
	notes = append(notes, n...)
	names := config.OldNames(os.Environ(), readIfExists(managedPath()))
	sort.Strings(names)
	for _, old := range names {
		notes = append(notes, old+" is set; rename it to LIVY_"+strings.TrimPrefix(old, oldnames.LivyEnv("")))
	}
	return notes
}
