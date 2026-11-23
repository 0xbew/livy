package hook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/0xbew/livy/internal/config"
	"github.com/0xbew/livy/internal/oldnames"
	"github.com/0xbew/livy/internal/pytext"
)

// Livy has no built-in org: the orgs in scope come from the managed file, the
// environment, the user file, or (until `livy migrate`) the Python tool's config.json,
// highest first. With none, nothing is in scope.

// dataDir is where the ledger lives: $LIVY_DIR, else the old names' variables, else
// ~/.livy (or the old user folder while only that exists).
func dataDir() string {
	dir, _ := resolveDataDir(os.Getenv, home(), pathExists)
	return dir
}

// legacyDir is the Python tool's data directory, whose config.json names its orgs; "" in a
// build with no Python tool to read.
func legacyDir() string {
	name := oldnames.RecorderDir()
	if name == "" {
		return ""
	}
	return envOrHome(oldnames.RecorderEnv("DIR"), name)
}

// envOrHome is the folder the variable env names, else name under the home folder.
func envOrHome(env, name string) string {
	if d := os.Getenv(env); d != "" {
		return d
	}
	return filepath.Join(home(), name)
}

// home is $HOME, else the account's home directory: never "", which would put the data
// dir and the user config inside whatever repo the hook runs in.
func home() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	return "/nonexistent"
}

func managedPath() string {
	p, _ := chooseManaged(runtime.GOOS, os.Getenv("ProgramData"), mayExist)
	return p
}

// mayExist is whether p may be there: only a path known to be missing is not, so a managed
// file whose folder cannot be searched is read (and refused) rather than passed over for the
// old name's one.
func mayExist(p string) bool {
	_, err := os.Stat(p)
	return !errors.Is(err, os.ErrNotExist)
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func readIfExists(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

// settings resolves the config layers: the managed file, the environment and the user file.
func settings() config.Resolved {
	managed, managedErr := readManaged()
	return config.Load(config.Inputs{Managed: managed, ManagedErr: managedErr,
		User: readIfExists(filepath.Join(userDir(home(), pathExists), "config.json")), Environ: os.Environ()})
}

// readManaged is the managed file's bytes, nil when there is none, and an error when it is
// there and cannot be read: read as absent, it would let the user file and env set what the
// admin's file locks.
func readManaged() ([]byte, error) {
	b, err := os.ReadFile(managedPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot be read: %w", err)
	}
	return b, nil
}

// orgs is the lowercased in-scope orgs. Like the Python ledger it is asked afresh on every
// scope check and says each time why a source named none.
func orgs(stderr io.Writer) []string { return orgsFrom(settings(), stderr) }

// orgsFrom is orgs for resolved settings res.
func orgsFrom(res config.Resolved, stderr io.Writer) []string {
	for _, p := range res.Refused {
		fmt.Fprintf(stderr, "ledger: %s\n", p)
	}
	if res.ManagedUnusable {
		return nil // the refused managed file may name the orgs: no other source may
	}
	if s, ok := res.Settings["scope.orgs"]; ok && (s.Layer == config.Managed || s.Layer == config.Env) {
		return lower(s.Value)
	}
	for _, name := range []string{config.EnvName("scope.orgs"), config.OldEnvName("scope.orgs"), oldnames.RecorderEnv("ORGS")} {
		if v, set := os.LookupEnv(name); name != "" && set && pytext.Strip(v) != "" && len(lower(strings.Split(v, ","))) == 0 {
			fmt.Fprintf(stderr, "ledger: %s names no org; ignoring it\n", name)
			break
		}
	}
	if s, ok := res.Settings["scope.orgs"]; ok {
		return lower(s.Value)
	}
	return legacyOrgs(stderr)
}

// hosts are the configured scope.hosts, lowercased; nil when unset (github.com). A setting
// that names no usable host fails closed (no host is in scope) and says so: falling back to
// github.com would record exactly what an admin who named another host meant to exclude.
func hosts(stderr io.Writer) []string {
	res := settings()
	s, ok := res.Settings["scope.hosts"]
	if !ok {
		return nil
	}
	v := s.Value
	if one, isStr := v.(string); isStr {
		v = []string{one}
	}
	out := lower(v)
	if len(out) == 0 {
		fmt.Fprintln(stderr, "ledger: scope.hosts names no host; no repository is in scope")
		return []string{}
	}
	return out
}

func lower(v any) []string {
	list, _ := v.([]string)
	var out []string
	for _, o := range list {
		if o = strings.ToLower(pytext.Strip(o)); o != "" {
			out = append(out, o)
		}
	}
	return out
}

// legacyOrgs reads `orgs` from the Python tool's config.json, as that tool reads it.
func legacyOrgs(stderr io.Writer) []string {
	if legacyDir() == "" {
		return nil
	}
	path := filepath.Join(legacyDir(), "config.json")
	if _, err := os.Stat(path); err != nil {
		return nil // as os.path.exists: absent, or a path that cannot be a file
	}
	raw, err := os.ReadFile(path)
	var loaded any
	if err == nil {
		err = json.Unmarshal(raw, &loaded)
	}
	if err != nil {
		fmt.Fprintf(stderr, "ledger: config.json unreadable (%v); no org is in scope\n", err)
		return nil
	}
	var out []string
	if m, ok := loaded.(map[string]any); ok {
		if list, ok := m["orgs"].([]any); ok {
			for _, v := range list {
				if s, ok := v.(string); ok && pytext.Strip(s) != "" {
					out = append(out, strings.ToLower(pytext.Strip(s)))
				}
			}
		}
	}
	if len(out) == 0 {
		fmt.Fprintln(stderr, "ledger: config.json has no usable `orgs`; no org is in scope")
	}
	return out
}
