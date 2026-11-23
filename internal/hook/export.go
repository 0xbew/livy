package hook

import (
	"io"
	"path/filepath"

	"github.com/0xbew/livy/internal/config"
	"github.com/0xbew/livy/internal/gitinfo"
	"github.com/0xbew/livy/internal/ledger"
)

// The other subcommands (emit, extract, install, doctor) share the hook's data folder,
// settings and ledger writer, so a line they write is checked and encoded exactly as a
// hook's is.

// DataDir is the folder the ledger lives in.
func DataDir() string { return dataDir() }

// Home is the user's home folder, never "".
func Home() string { return home() }

// UserConfigPath is the user layer's config file.
func UserConfigPath() string { return filepath.Join(userDir(home(), pathExists), "config.json") }

// ManagedPath is the admin's managed file for this OS.
func ManagedPath() string { return managedPath() }

// Settings resolves the config layers as the hooks do; a refused file is in Refused.
func Settings() config.Resolved { return settings() }

// Orgs is the lowercased orgs in scope; why a source named none goes to stderr.
func Orgs(stderr io.Writer) []string { return orgs(stderr) }

// WriteError appends one ledger.error with repo "unknown", for a subcommand that runs
// outside any one repository. When even that cannot be written, stderr says so.
func WriteError(reason string, stderr io.Writer) {
	r := &run{stderr: stderr, git: gitinfo.New(stderr), path: filepath.Join(dataDir(), ledger.FileName)}
	r.errorLine(reason, false, false)
}

// Append validates rec against the schema, encodes it as a hook line, and appends it to
// the ledger. False means it was written nowhere.
func Append(rec map[string]any, stderr io.Writer) bool {
	r := &run{stderr: stderr, git: gitinfo.New(stderr), path: filepath.Join(dataDir(), ledger.FileName)}
	return r.append(rec)
}

// LegacyDir is the Python tool's data folder, where its config and key files are.
func LegacyDir() string { return legacyDir() }
