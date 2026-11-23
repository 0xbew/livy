// Package oldnames is the names a machine's earlier recorders used, which Livy still reads
// so a machine moves over without losing a setting: Livy's own name before it was renamed,
// and the Python recorder it replaced. They are set only in the private build (known.go,
// which the public export leaves out). In the public build every name is empty, and every
// reader skips an empty one: there is no old name to read.
package oldnames

import (
	"os"
	"strings"
)

var (
	// Livy is Livy's name before the rename. Its variables were LIVY's with this prefix, its
	// user folder ~/.<lower case>, and its managed file sat in a folder of this name.
	Livy string
	// Recorder is the Python recorder's name: its variables are this, upper case with "_" for
	// "-", plus "_" and a suffix; its data folder is ~/.<Recorder>.
	Recorder string
	// Launcher is the prefix of the launcher tags the Python recorder read (<Launcher>_AGENT_ROLE).
	Launcher string
	// Label is the prefix of the Python recorder's launchd labels (<Label>.daily).
	Label string
	// Roles are agent_role values a private launcher sets, beside the public ones.
	Roles []string
)

// LivyEnv is the variable Livy read for suffix under its old name; "" when there is none.
func LivyEnv(suffix string) string { return envName(Livy, suffix) }

// RecorderEnv is the Python recorder's variable for suffix; "" when there is none.
func RecorderEnv(suffix string) string { return envName(Recorder, suffix) }

// LauncherEnv is the Python recorder's launcher tag for suffix; "" when there is none.
func LauncherEnv(suffix string) string { return envName(Launcher, suffix) }

func envName(prefix, suffix string) string {
	if prefix == "" {
		return ""
	}
	return strings.ToUpper(strings.ReplaceAll(prefix, "-", "_")) + "_" + suffix
}

// LivyDir is the old user folder's name under home (".name"); "" when there is none.
func LivyDir() string { return dotName(strings.ToLower(Livy)) }

// RecorderDir is the Python recorder's data folder's name under home; "" when there is none.
func RecorderDir() string { return dotName(Recorder) }

func dotName(name string) string {
	if name == "" {
		return ""
	}
	return "." + name
}

// Getenv is the value of an old variable, "" when the variable or its name is empty: the
// public build has no old names, and an empty name must never be looked up.
func Getenv(name string) string {
	if name == "" {
		return ""
	}
	return os.Getenv(name)
}
