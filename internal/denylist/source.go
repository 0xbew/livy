// Package denylist keeps the machine's copy of the identity denylist current from the
// source the config names, and says how that copy stands.
//
// The copy (<data dir>/denylist.json unless denylist.path says otherwise) is what every
// sender scrubs with: the source's payload plus its "version". A refresh replaces it only
// with a whole valid version, written atomically at mode 0600; a failed fetch, an empty
// answer, or a payload that is not a valid denylist leaves the copy as it was, and says so.
// Each refresh is recorded in <data dir>/denylist-refresh.json (time, outcome, version,
// reason), never with the list's text, so doctor can tell a stopped refresh from a quiet one.
//
// No source is built in: with no denylist.source the copy is whatever was put there by hand.
package denylist

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/0xbew/livy/internal/config"
	"github.com/0xbew/livy/internal/pytext"
	"github.com/0xbew/livy/internal/sink/gcs"
	"github.com/0xbew/livy/internal/sink/s3"
)

// FileName is the copy's name in the data folder; StateName is the refresh record's.
const (
	FileName  = "denylist.json"
	StateName = "denylist-refresh.json"
)

// DataNames are the files and folders this package keeps in the data folder, by exact name.
// `livy uninstall --purge` deletes these and nothing else.
func DataNames() []string { return []string{FileName, StateName} }

// Source is the `denylist.source` section:
//
//	{"denylist": {"source": {"type": "file", "path": "/abs/denylist.json"}}}
//	{"denylist": {"source": {"type": "gcs", "bucket": "b", "object": "path/denylist.json",
//	              "auth": "gcloud|adc|command", "writer_sa": "...", "account": "...",
//	              "token_command": ["..."], "endpoint": "", "gcloud": ""}}}
//	{"denylist": {"source": {"type": "s3", "bucket": "b", "object": "path/denylist.json",
//	              "region": "", "profile": "", "credential_command": ["..."], "endpoint": ""}}}
//	{"denylist": {"source": {"type": "secretmanager", "secret": "name", "project": "id",
//	              ...the gcs credential keys...}}}
//
// A file or object holds the copy's own shape (the payload plus "version"); a secret holds
// the payload, and its version is the secret version's number.
type Source struct {
	Type    string
	Path    string // file
	Bucket  string // gcs, s3
	Object  string // gcs, s3
	Secret  string // secretmanager
	Project string // secretmanager
	GCS     gcs.Config
	S3      s3.Config
}

const section = "denylist.source"

// FromSettings reads denylist.source. nil, nil when no source is configured. A key of the
// wrong type is refused, never read as empty, as the sink's are.
func FromSettings(res config.Resolved) (*Source, error) {
	var bad []string
	str := func(k string) string {
		v, ok := res.Settings[section+"."+k]
		s, isStr := v.Value.(string)
		if ok && !isStr {
			bad = append(bad, section+"."+k+" must be a string")
		}
		return pytext.Strip(s)
	}
	list := func(k string) []string {
		v, ok := res.Settings[section+"."+k]
		l, isList := v.Value.([]string)
		if ok && !isList {
			bad = append(bad, section+"."+k+" must be a list of strings")
		}
		return l
	}
	s := &Source{Type: str("type"), Path: str("path"), Bucket: str("bucket"), Object: str("object"),
		Secret: str("secret"), Project: str("project")}
	s.GCS = gcs.Config{Bucket: s.Bucket, Endpoint: str("endpoint"), Auth: str("auth"), WriterSA: str("writer_sa"),
		Account: str("account"), TokenCommand: list("token_command"), Gcloud: str("gcloud")}
	s.S3 = s3.Config{Bucket: s.Bucket, Region: str("region"), Endpoint: str("endpoint"), Profile: str("profile"),
		CredentialCommand: list("credential_command")}
	if len(bad) > 0 {
		return nil, errors.New(strings.Join(bad, "; "))
	}
	switch s.Type {
	case "":
		return nil, nil
	case "file":
		if !filepath.IsAbs(s.Path) {
			return nil, errors.New(section + ".path must be an absolute path")
		}
	case "gcs", "s3":
		if s.Bucket == "" || s.Object == "" {
			return nil, fmt.Errorf("%s.bucket and %s.object must name the %s object", section, section, s.Type)
		}
	case "secretmanager":
		if s.Secret == "" || s.Project == "" {
			return nil, fmt.Errorf("%s.secret and %s.project must name the secret", section, section)
		}
	default:
		return nil, fmt.Errorf("%s.type %q is not one of file, gcs, s3, secretmanager", section, s.Type)
	}
	return s, nil
}

// Path is the local copy: denylist.path, else <dataDir>/denylist.json. Every reader and the
// refresh use this one answer.
func Path(res config.Resolved, dataDir string) string {
	if s, ok := res.Settings["denylist.path"]; ok {
		if v, isStr := s.Value.(string); isStr && pytext.Strip(v) != "" {
			return v
		}
	}
	return filepath.Join(dataDir, FileName)
}

// remote is a source whose copy should be re-confirmed daily.
func (s *Source) remote() bool { return s != nil && s.Type != "file" }
