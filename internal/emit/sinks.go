package emit

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/0xbew/livy/internal/config"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/sink/gcs"
	"github.com/0xbew/livy/internal/sink/git"
	"github.com/0xbew/livy/internal/sink/local"
	"github.com/0xbew/livy/internal/sink/s3"
)

// ErrNoSink is a machine with no sink configured: nothing is uploaded and the ledger is kept.
// It is a state, not a failure: emit writes no ledger.error for it, and doctor warns.
var ErrNoSink = errors.New("no sink configured (sink.type)")

// ErrManagedRefused is a machine whose managed config file is refused: it may name the sink,
// so no other layer's sink is used and nothing is uploaded until it is fixed.
var ErrManagedRefused = errors.New("the managed config file is refused and may lock the sink")

// FromSettings builds the configured sink from the `sink` section of the config:
//
//	{"sink": {"type": "local", "path": "/abs/folder"}}
//	{"sink": {"type": "gcs", "bucket": "b", "prefix": "", "auth": "gcloud|adc|command",
//	          "writer_sa": "...", "account": "...", "token_command": ["..."], "endpoint": "",
//	          "gcloud": "/path/to/gcloud"}}
//	{"sink": {"type": "s3", "bucket": "b", "prefix": "", "region": "us-east-1", "profile": "",
//	          "credential_command": ["..."], "endpoint": ""}}
//	{"sink": {"type": "git", "remote": "https://github.com/owner/repo.git", "branch": "main"}}
//
// A key of the wrong type is refused, never read as empty: an empty token_command would
// quietly switch how the sink authenticates.
func FromSettings(res config.Resolved) (Sink, error) { return fromSection(res, "sink") }

// gitCloneDir is the git sink's own clone, inside the data folder.
const gitCloneDir = "sink-git"

// ErrNoPromptSink is a machine with prompt upload on and no prompts.sink: the text stays queued.
var ErrNoPromptSink = errors.New("no prompt sink configured (prompts.sink.type)")

// ErrSamePromptSink refuses a prompt sink that names the ledger's place: prompt text never goes
// through the ledger's sink.
var ErrSamePromptSink = errors.New("prompts.sink is the same place as sink; prompt text needs its own bucket or folder")

// PromptSinkFromSettings builds the `prompts.sink` section, which has the same keys as
// `sink` and no default.
func PromptSinkFromSettings(res config.Resolved) (Sink, error) {
	if res.ManagedUnusable {
		return nil, ErrManagedRefused
	}
	get := func(section, k string) string { s, _ := res.Settings[section+"."+k].Value.(string); return s }
	if get("prompts.sink", "type") == "" {
		return nil, ErrNoPromptSink
	}
	if samePlace(get) {
		return nil, ErrSamePromptSink
	}
	return fromSection(res, "prompts.sink")
}

// samePlace is whether the prompt sink names any of the ledger's place: the same bucket whatever
// the prefix, scheme, case or slashes, or, for folders, the same one or one inside the other.
func samePlace(get func(section, k string) string) bool {
	lt, pt := strings.ToLower(strings.TrimSpace(get("sink", "type"))), strings.ToLower(strings.TrimSpace(get("prompts.sink", "type")))
	if lt != pt {
		return false
	}
	switch lt {
	case "gcs", "s3":
		return bucketName(get("sink", "bucket")) == bucketName(get("prompts.sink", "bucket"))
	case "local":
		a, b := folder(get("sink", "path")), folder(get("prompts.sink", "path"))
		return inside(a, b) || inside(b, a)
	case "git":
		return remoteName(get("sink", "remote")) == remoteName(get("prompts.sink", "remote"))
	}
	return true // a type fromSection will refuse anyway
}

func bucketName(b string) string {
	b = strings.ToLower(strings.TrimSpace(b))
	b = strings.TrimPrefix(strings.TrimPrefix(b, "gs://"), "s3://")
	name, _, _ := strings.Cut(strings.Trim(b, "/"), "/")
	return name
}

// remoteName is a git remote folded for comparison: case, a trailing slash and ".git" ignored.
func remoteName(r string) string {
	r = strings.TrimSuffix(strings.TrimRight(strings.ToLower(strings.TrimSpace(r)), "/"), ".git")
	return strings.TrimRight(r, "/")
}

// inside is whether a is b or lies under it.
func inside(a, b string) bool {
	rel, err := filepath.Rel(b, a)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// folder is a path made absolute, with symlinks resolved through its nearest existing
// ancestor (a folder not made yet still compares with the real one), and folded to lower case
// where the filesystem is usually case-insensitive.
func folder(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; cur = filepath.Dir(cur) {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			p = filepath.Join(real, rest)
			break
		}
		if cur == filepath.Dir(cur) {
			break
		}
		rest = filepath.Join(filepath.Base(cur), rest)
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func fromSection(res config.Resolved, section string) (Sink, error) {
	if res.ManagedUnusable {
		return nil, ErrManagedRefused
	}
	var bad []string
	str := func(k string) string {
		v, ok := res.Settings[section+"."+k]
		s, isStr := v.Value.(string)
		if ok && !isStr {
			bad = append(bad, section+"."+k+" must be a string")
		}
		return s
	}
	list := func(k string) []string {
		v, ok := res.Settings[section+"."+k]
		l, isList := v.Value.([]string)
		if ok && !isList {
			bad = append(bad, section+"."+k+" must be a list of strings")
		}
		return l
	}
	if err := typeMissing(res, section); err != nil {
		return nil, err
	}
	sink, err := build(str, list)
	if len(bad) > 0 {
		return nil, errors.New(strings.Join(bad, "; "))
	}
	return sink, err
}

// typeMissing refuses a section that sets keys but no type: a half-written sink is a broken
// one, not a machine that keeps its data local (ErrNoSink).
func typeMissing(res config.Resolved, section string) error {
	if t, _ := res.Settings[section+".type"].Value.(string); t != "" {
		return nil
	}
	var set []string
	for k, v := range res.Settings {
		if s, isStr := v.Value.(string); isStr && strings.TrimSpace(s) == "" {
			continue
		}
		if strings.HasPrefix(k, section+".") && k != section+".type" {
			set = append(set, k)
		}
	}
	if len(set) == 0 {
		return nil
	}
	slices.Sort(set)
	return fmt.Errorf("%s.type is missing but %s is set", section, strings.Join(set, ", "))
}

func build(str func(string) string, list func(string) []string) (Sink, error) {
	switch t := str("type"); t {
	case "git":
		return git.New(git.Config{Remote: str("remote"), Branch: str("branch"), Dir: filepath.Join(hook.DataDir(), gitCloneDir)})
	case "":
		return nil, ErrNoSink
	case "local":
		return local.New(str("path"))
	case "gcs":
		return gcs.New(gcsConfig(str, list))
	case "s3":
		return s3.New(s3.Config{Bucket: str("bucket"), Prefix: str("prefix"), Region: str("region"),
			Endpoint: str("endpoint"), Profile: str("profile"), CredentialCommand: list("credential_command")})
	default:
		return nil, fmt.Errorf("sink.type %q is not one of local, gcs, s3, git", t)
	}
}

func gcsConfig(str func(string) string, list func(string) []string) gcs.Config {
	return gcs.Config{Bucket: str("bucket"), Prefix: str("prefix"), Endpoint: str("endpoint"),
		Auth: str("auth"), WriterSA: str("writer_sa"), Account: str("account"),
		TokenCommand: list("token_command"), Gcloud: str("gcloud")}
}

// GCSConfig is the sink section read as a gcs sink's settings, for the OTEL ID token, which
// is minted as the same writer. A sink of another type is refused.
func GCSConfig(res config.Resolved) (gcs.Config, error) {
	str := func(k string) string { s, _ := res.Settings["sink."+k].Value.(string); return s }
	list := func(k string) []string { l, _ := res.Settings["sink."+k].Value.([]string); return l }
	if t := str("type"); t != "gcs" {
		return gcs.Config{}, fmt.Errorf("sink.type is %q: an ID token is minted as a gcs sink's writer", t)
	}
	return gcsConfig(str, list), nil
}
