package prompts

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xbew/scrub"

	"github.com/0xbew/livy/internal/config"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/pytext"
)

const (
	// DenylistFile is the machine's identity list in the data folder, unless denylist.path.
	DenylistFile = "denylist.json"
	uploadKey    = "prompts.upload"
	// legacyUploadKey is the Python tool's top-level key, honoured so a migrated machine keeps its opt-in.
	legacyUploadKey = "prompt_upload"
	configLock      = "config.json.lock"
)

// UploadOn is whether the engineer opted in on this machine. Off unless the user config
// (or, on a migrated machine, the Python tool's config.json) says true; a managed or
// environment value is not read, so only the engineer turns it on.
func UploadOn() bool {
	if v, ok := userSettings()[uploadKey].(bool); ok {
		return v
	}
	legacy := hook.LegacyDir()
	if legacy == "" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(legacy, "config.json"))
	if err != nil {
		return false
	}
	flat, err := config.Flatten(raw)
	if err != nil {
		return false
	}
	v, _ := flat[legacyUploadKey].(bool)
	return v
}

func userSettings() map[string]any {
	raw, err := os.ReadFile(hook.UserConfigPath())
	if err != nil {
		return nil
	}
	flat, err := config.Flatten(raw)
	if err != nil {
		return nil
	}
	return flat
}

// SetUpload writes the opt-in into the user config, keeping every other key. A config that
// cannot be read is refused and left as it is.
func SetUpload(on bool) error {
	path := hook.UserConfigPath()
	release, err := ledger.Lock(path+".lock", 10*time.Second)
	if err != nil {
		return errors.New("config.json is locked by another writer")
	}
	defer release()
	top := map[string]any{}
	switch raw, err := os.ReadFile(path); {
	case err == nil:
		if _, ferr := config.Flatten(raw); ferr != nil {
			return errors.New("config.json cannot be read: " + ferr.Error())
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		_ = dec.Decode(&top)
	case !errors.Is(err, os.ErrNotExist):
		return errors.New("config.json cannot be read")
	}
	section, _ := top["prompts"].(map[string]any)
	if section == nil {
		section = map[string]any{}
	}
	section["upload"], top["prompts"] = on, section
	out, _ := json.MarshalIndent(top, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func denylistPath() string {
	if s, ok := hook.Settings().Settings["denylist.path"]; ok {
		if v, isStr := s.Value.(string); isStr && pytext.Strip(v) != "" {
			return v
		}
	}
	return filepath.Join(hook.DataDir(), DenylistFile)
}

// loadDenylist is the local copy, or why there is none. Never scrub.NoDenylist: without
// names nothing is written.
func LoadDenylist() (*scrub.Denylist, string) {
	d, err := scrub.LoadDenylist(denylistPath())
	if err != nil {
		return nil, err.Error()
	}
	return d, ""
}

// Denylist reports whether a usable denylist exists, and why not (doctor).
func Denylist() (ok bool, why string) {
	d, why := LoadDenylist()
	return d != nil, why
}

func lowerNoSpace(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
