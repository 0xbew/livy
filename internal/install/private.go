package install

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/0xbew/livy/internal/hook"
)

// privateMode is the mode of every folder Livy keeps captured data in: what it records names
// repos, branches and sessions, which other users on the machine must not read.
const privateMode fs.FileMode = 0o700

// PrivacyNote is one line MakePrivate reports. Failed is a folder it could not create, check
// or make private: the caller fails, since what Livy records there would be left readable.
type PrivacyNote struct {
	Text   string
	Failed bool
}

// MakePrivate creates each folder 0700, and makes an existing one 0700 when other users can
// open it and it is Livy's own. A folder the user pointed Livy at (LIVY_DIR) may be shared on
// purpose, so it is reported, not changed. Each note says what was done or why not.
func MakePrivate(own []string, shared string, dry bool) (notes []PrivacyNote) {
	say := func(format string, a ...any) { notes = append(notes, PrivacyNote{Text: fmt.Sprintf(format, a...)}) }
	fail := func(format string, a ...any) {
		notes = append(notes, PrivacyNote{Text: fmt.Sprintf(format, a...), Failed: true})
	}
	dirs := slices.Clone(own)
	if shared != "" && !slices.Contains(dirs, filepath.Clean(shared)) {
		dirs = append(dirs, shared)
	}
	for _, dir := range dirs {
		mine := slices.Contains(own, dir)
		st, err := os.Stat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist) && dry:
		case errors.Is(err, fs.ErrNotExist):
			if err := os.MkdirAll(dir, privateMode); err != nil {
				fail("could not create %s: %v", dir, err)
			}
		case err != nil:
			fail("could not check %s: %v", dir, err)
		case st.Mode().Perm()&0o077 == 0:
		case !mine:
			say("WARNING: %s is %#o, so other users can read what Livy records; chmod 700 it", dir, st.Mode().Perm())
		case dry:
			say("would make %s private (it is %#o)", dir, st.Mode().Perm())
		default:
			if err := os.Chmod(dir, privateMode); err != nil {
				fail("could not make %s private (it is %#o): %v", dir, st.Mode().Perm(), err)
				continue
			}
			say("WARNING: %s was %#o, readable by other users; made it 0700", dir, st.Mode().Perm())
		}
	}
	return notes
}

// ownFolders are the folders that are Livy's alone: ~/.livy (where the binary lives), the
// user folder, and the data folder when it is one of those.
func ownFolders(p paths, data string) (own []string, shared string) {
	own = []string{filepath.Clean(filepath.Dir(filepath.Dir(p.bin)))}
	if u := filepath.Clean(p.userDir); !slices.Contains(own, u) {
		own = append(own, u)
	}
	if d := filepath.Clean(data); !slices.Contains(own, d) {
		return own, d
	}
	return own, ""
}

// FixPrivate is MakePrivate for doctor, over this machine's folders that exist: a check
// creates nothing, and creating ~/.livy would move a machine still on Livy's old folder off it.
func FixPrivate() []PrivacyNote {
	own, shared := ownFolders(where(), hook.DataDir())
	own = slices.DeleteFunc(own, absent)
	if absent(shared) {
		shared = ""
	}
	return MakePrivate(own, shared, false)
}

func absent(dir string) bool {
	_, err := os.Stat(dir)
	return dir == "" || errors.Is(err, fs.ErrNotExist)
}
