package spotcheck

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0xbew/scrub"

	"github.com/0xbew/livy/internal/ledgerjson"
	"github.com/0xbew/livy/internal/pytext"
)

const (
	// KeyFile sits in the data folder, mode 0600.
	KeyFile   = "jev.key"
	pauseFile = "spotcheck.pause"

	pauseAfterAuth  = time.Hour
	pauseAfterError = 5 * time.Minute
)

// DataNames are the files and folders this package keeps in the data folder, by exact name.
// `livy uninstall --purge` deletes these and nothing else.
func DataNames() []string { return []string{KeyFile, pauseFile} }

var keyEnv = []string{"JEV_API_KEY", "TYPESAFE_API_KEY"}

// APIKey is the Jev key, or "" when there is none and the checks are off: the environment
// first, then jev.key in each folder given (the data folder, then the Python tool's, so an
// install that migrated keeps its key).
func APIKey(getenv func(string) string, dirs ...string) string {
	for _, name := range keyEnv {
		if v := pytext.Strip(getenv(name)); v != "" {
			return v
		}
	}
	for _, dir := range dirs {
		if raw, err := os.ReadFile(filepath.Join(dir, KeyFile)); err == nil {
			if v := pytext.Strip(string(raw)); v != "" {
				return v
			}
		}
	}
	return ""
}

// Denylist is the machine's identity list as the checks use it.
type Denylist struct {
	D *scrub.Denylist
	// Version is the list's version; nil with no local list, so a receipt then carries no
	// denylist_version.
	Version *int
	// Problem is why a list that exists cannot be used. Nothing is sent then.
	Problem string
}

// LoadDenylist reads the local list. No list at all is no problem: identities are then
// not scrubbed. A list that cannot be used, or one whose entry matches the fixed question
// text (which would refuse every check), is a Problem.
func LoadDenylist(path string) Denylist {
	d, err := scrub.LoadDenylist(path)
	if err != nil {
		if errors.Is(err, scrub.ErrDenylistMissing) {
			return Denylist{D: scrub.NoDenylist}
		}
		return Denylist{D: scrub.NoDenylist, Problem: err.Error()}
	}
	var frame any
	all := obj{{"model", Model}, {"questions", obj{{"guard@1", contracts["guard@1"]}, {"route@1", contracts["route@1"]}}}}
	_ = json.Unmarshal([]byte(dumps(all)), &frame)
	if hits := scrub.AuditValue(frame, d); len(hits) > 0 {
		return Denylist{D: scrub.NoDenylist,
			Problem: strings.Join(hits, ", ") + " match Livy's own question text, so every check would be refused"}
	}
	v := d.Version
	return Denylist{D: d, Version: &v}
}

// Paused reports whether checks are paused after a failure.
func Paused(dir string, now time.Time) bool {
	raw, err := os.ReadFile(filepath.Join(dir, pauseFile))
	if err != nil {
		return false
	}
	var data struct {
		Until float64 `json:"until"`
	}
	if json.Unmarshal(raw, &data) != nil || data.Until == 0 {
		return false
	}
	return data.Until > float64(now.UnixNano())/1e9
}

// Pause starts a pause; false means it could not be recorded, so the next call tries again.
func Pause(dir string, d time.Duration, reason string, now time.Time) bool {
	path := filepath.Join(dir, pauseFile)
	if os.MkdirAll(dir, 0o700) != nil {
		return false
	}
	until := float64(now.Unix()) + float64(now.Nanosecond())/1e9 + d.Seconds()
	r, _ := ledgerjson.Marshal(reason)
	content := `{"until": ` + ledgerjson.FormatFloat(until) + `, "reason": ` + string(r) + `}`
	tmp := path + "." + strconv.Itoa(os.Getpid())
	if os.WriteFile(tmp, []byte(content), 0o600) != nil {
		return false
	}
	if os.Rename(tmp, path) != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}
