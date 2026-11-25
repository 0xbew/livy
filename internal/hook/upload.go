package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/0xbew/livy/internal/config"
)

// The user file's sinks say where the installed data folder's data goes. A data folder
// LIVY_DIR points elsewhere (a scratch folder for a probe or a test) is not that data: it ships
// only to a sink its own config.json names, never to the user's.

// installRecord is the one field of `livy install`'s record that uploads read.
const installRecord = "install.json"

// UploadSettings resolves the config layers for an upload (emit, drain and the prompt stream):
// as Settings, except that a data folder other than the installed one takes its sinks from its
// own config.json, with the managed file and the environment over it as usual.
func UploadSettings() config.Resolved {
	res := settings()
	dir := dataDir()
	if installed, _ := InstalledDataDir(dir); installed {
		return res
	}
	managed, managedErr := readManaged()
	own := config.Load(config.Inputs{Managed: managed, ManagedErr: managedErr,
		User: readIfExists(filepath.Join(dir, "config.json")), Environ: os.Environ()})
	for k := range res.Settings {
		if isSinkKey(k) {
			delete(res.Settings, k)
		}
	}
	for k, v := range own.Settings {
		if isSinkKey(k) {
			res.Settings[k] = v
		}
	}
	return res
}

// InstalledDataDir says whether dir is the data folder `livy install` set up: the user folder,
// or the one its record names. recorded is the record's folder ("" when it names none).
func InstalledDataDir(dir string) (installed bool, recorded string) {
	user := userDir(home(), pathExists)
	var rec struct {
		DataDir string `json:"data_dir"`
	}
	// An unreadable record names no folder: only the user folder is then installed.
	_ = json.Unmarshal(readIfExists(filepath.Join(user, installRecord)), &rec)
	return sameDir(dir, user) || (rec.DataDir != "" && sameDir(dir, rec.DataDir)), rec.DataDir
}

// isSinkKey says whether a flattened key is part of a sink section: the ledger's or the prompt
// text's.
func isSinkKey(k string) bool {
	return strings.HasPrefix(k, "sink.") || strings.HasPrefix(k, "prompts.sink.")
}

// sameDir says whether a and b are one folder: the same file when both exist, else the same
// cleaned absolute path.
func sameDir(a, b string) bool {
	sa, errA := os.Stat(a)
	sb, errB := os.Stat(b)
	if errA == nil && errB == nil {
		return os.SameFile(sa, sb)
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	return errA == nil && errB == nil && absA == absB
}
