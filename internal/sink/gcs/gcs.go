// Package gcs is the Google Cloud Storage sink: each object is created with the JSON API's
// ifGenerationMatch=0 precondition, so it never replaces one, and an object already there
// (HTTP 412) is reported as fs.ErrExist. Only create permission is needed: `gcloud storage
// cp` with a precondition first asks whether the object exists, which a create-only writer
// may not do.
//
// Auth is keyless and nothing is stored: a short-lived token is minted per Sink (one per
// emit pass) and kept in memory only. The machine's gcloud configuration is never changed.
package gcs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/0xbew/livy/internal/oldnames"
)

// Config is the `sink` section for type gcs.
//
//	{"sink": {"type": "gcs", "bucket": "my-bucket", "prefix": "optional/path",
//	          "auth": "gcloud" | "adc" | "command",
//	          "writer_sa": "writer@project.iam.gserviceaccount.com", "account": "me@example.com",
//	          "token_command": ["my-token-printer", "--flag"],
//	          "endpoint": "http://localhost:4443"}}
type Config struct {
	Bucket   string
	Prefix   string
	Endpoint string // default https://storage.googleapis.com; set for an emulator
	// Auth picks how the token is minted: gcloud (`gcloud auth print-access-token`, the
	// default when gcloud is installed), adc (Application Default Credentials, read natively),
	// or command (TokenCommand prints a token).
	Auth         string
	WriterSA     string // impersonated with gcloud or adc; "" uses the identity as-is
	Account      string // gcloud only: which of the engineer's gcloud identities to act as
	TokenCommand []string
	Gcloud       string // gcloud's path; "" searches PATH and the usual install folders
}

// Sink uploads to one bucket.
type Sink struct {
	c      Config
	client *http.Client
	scope  string // what an ADC impersonation asks for
	mu     sync.Mutex
	token  string
}

// writerAsIs is the Python tool's writer_sa value for "use the signed-in
// account as it is", which is not a service account's name.
const writerAsIs = "active"

const (
	defaultEndpoint = "https://storage.googleapis.com"
	tokenTimeout    = 60 * time.Second
)

// New checks the config; nothing is called until the first Put.
func New(c Config) (*Sink, error) {
	c.Bucket = strings.TrimSuffix(strings.TrimPrefix(c.Bucket, "gs://"), "/")
	if c.Bucket == "" || strings.Contains(c.Bucket, "/") {
		return nil, fmt.Errorf("sink.bucket must name a bucket, got %q", c.Bucket)
	}
	c.Prefix = strings.Trim(c.Prefix, "/")
	if c.WriterSA == writerAsIs {
		c.WriterSA = ""
	}
	if c.Endpoint == "" {
		c.Endpoint = defaultEndpoint
	}
	c.Endpoint = strings.TrimSuffix(c.Endpoint, "/")
	if err := c.settleAuth("sink"); err != nil {
		return nil, err
	}
	return &Sink{c: c, client: noRedirects(), scope: storageScope}, nil
}

// settleAuth picks the default auth and checks it; section names the config keys in errors.
func (c *Config) settleAuth(section string) error {
	if c.Auth == "" {
		c.Auth = "adc"
		if c.Gcloud != "" || findGcloud() != "" {
			c.Auth = "gcloud"
		}
	}
	switch c.Auth {
	case "gcloud", "adc":
	case "command":
		if len(c.TokenCommand) == 0 {
			return fmt.Errorf("%s.auth is command but %s.token_command is empty", section, section)
		}
	default:
		return fmt.Errorf("%s.auth %q: want gcloud, adc or command", section, c.Auth)
	}
	return nil
}

// noRedirects never follows a redirect: Go turns a redirected POST into a GET, whose 200
// could read as "stored". A 3xx is a failure and the copy is kept.
func noRedirects() *http.Client {
	return &http.Client{Timeout: 2 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (s *Sink) name(key string) string {
	if s.c.Prefix == "" {
		return key
	}
	return s.c.Prefix + "/" + key
}

// URL is gs://bucket/[prefix/]key.
func (s *Sink) URL(key string) string { return "gs://" + s.c.Bucket + "/" + s.name(key) }

// Put creates the object, never replacing one. Only a 412 whose reason is conditionNotMet
// means the object is already there (its key is the hash of these bytes, so it holds
// them), and only a 2xx naming the object means it was stored: either answer lets the
// caller delete its copy, so anything less certain is a failure and the copy is kept.
func (s *Sink) Put(ctx context.Context, key string, body []byte) error {
	status, detail, err := s.post(ctx, key, body)
	if err == nil && status == http.StatusUnauthorized {
		s.mu.Lock()
		s.token = "" // expired mid-pass: mint once more
		s.mu.Unlock()
		status, detail, err = s.post(ctx, key, body)
	}
	if err != nil {
		return err
	}
	switch {
	case status >= 200 && status < 300:
		var obj struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(detail, &obj) != nil || obj.Name != s.name(key) {
			return fmt.Errorf("upload to %s: HTTP %d without the stored object in the answer", s.URL(key), status)
		}
		return nil
	case status == http.StatusPreconditionFailed && conditionNotMet(detail):
		return fmt.Errorf("%s: %w", s.URL(key), fs.ErrExist)
	default:
		return fmt.Errorf("upload to %s: HTTP %d %s", s.URL(key), status, short(detail))
	}
}

func (s *Sink) post(ctx context.Context, key string, body []byte) (int, []byte, error) {
	token, err := s.accessToken(ctx)
	if err != nil {
		return 0, nil, err
	}
	u := s.c.Endpoint + "/upload/storage/v1/b/" + url.PathEscape(s.c.Bucket) +
		"/o?uploadType=media&ifGenerationMatch=0&name=" + url.QueryEscape(s.name(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("upload to %s: %w", s.URL(key), err)
	}
	defer resp.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return resp.StatusCode, detail, nil
}

// conditionNotMet is a GCS error body whose reasons are all conditionNotMet: the
// precondition failed, so the object exists. Anything else is not proof of that.
func conditionNotMet(body []byte) bool {
	var e struct {
		Error struct {
			Code   int `json:"code"`
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error.Code != http.StatusPreconditionFailed || len(e.Error.Errors) == 0 {
		return false
	}
	for _, x := range e.Error.Errors {
		if x.Reason != "conditionNotMet" {
			return false
		}
	}
	return true
}

func short(b []byte) string {
	t := strings.Join(strings.Fields(string(b)), " ")
	if len(t) > 200 {
		t = t[:200]
	}
	return t
}

// accessToken mints the pass's token once.
func (s *Sink) accessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" {
		return s.token, nil
	}
	var tok string
	var err error
	switch s.c.Auth {
	case "command":
		tok, err = runToken(ctx, s.c.TokenCommand)
	case "gcloud":
		tok, err = s.gcloudToken(ctx)
	default:
		tok, err = adcToken(ctx, s.client, s.c.WriterSA, s.scope)
	}
	if err != nil {
		return "", err
	}
	s.token = tok
	return tok, nil
}

// gcloudToken asks gcloud for a token as the writer. It names the account and the writer on
// the one command and never runs `gcloud config set`: an engineer who switches accounts for
// other work does not break uploads, and uploads never switch it back.
func (s *Sink) gcloudToken(ctx context.Context) (string, error) {
	g := s.c.Gcloud
	if g == "" {
		g = findGcloud()
	}
	if g == "" {
		return "", errors.New("gcloud not found (PATH or the usual install folders); set sink.auth to adc or command")
	}
	args := []string{g, "auth", "print-access-token"}
	if s.c.Account != "" {
		args = append(args, "--account="+s.c.Account)
	}
	if s.c.WriterSA != "" {
		args = append(args, "--impersonate-service-account="+s.c.WriterSA)
	}
	tok, err := runToken(ctx, args)
	if err != nil && s.c.WriterSA != "" {
		return "", fmt.Errorf("could not act as %s: %w", s.c.WriterSA, err)
	}
	return tok, err
}

// IDToken is an OIDC ID token for audience, minted as the writer through gcloud: what a
// Cloud Run collector that only the writer may invoke accepts (`livy otel-headers --gcloud`).
// gcloud mints an ID token for an audience only as a service account, so a writer is
// required; gcloud mints it whatever sink.auth the uploads use. Nothing is stored; the machine's
// gcloud configuration is never changed.
func IDToken(ctx context.Context, c Config, audience string) (string, error) {
	if c.WriterSA == "" {
		return "", errors.New("no sink.writer_sa: gcloud mints an ID token for an audience only as a service account")
	}
	g := c.Gcloud
	if g == "" {
		g = findGcloud()
	}
	if g == "" {
		return "", errors.New("gcloud not found (PATH or the usual install folders)")
	}
	args := []string{g, "auth", "print-identity-token", "--impersonate-service-account=" + c.WriterSA,
		"--audiences=" + strings.TrimSuffix(audience, "/"), "--include-email"}
	if c.Account != "" {
		args = append(args, "--account="+c.Account)
	}
	tok, err := runToken(ctx, args)
	if err != nil {
		return "", fmt.Errorf("could not act as %s: %w", c.WriterSA, err)
	}
	return tok, nil
}

// runToken runs a command with no stdin (a re-auth prompt must fail, not hang) and returns
// its trimmed stdout; on failure, the last line of its stderr.
func runToken(ctx context.Context, argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	tok := strings.TrimSpace(string(out))
	if err != nil || tok == "" {
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		why := lines[len(lines)-1]
		if why == "" && err != nil {
			why = err.Error()
		}
		if why == "" {
			why = "no token printed"
		}
		return "", fmt.Errorf("%s: %s", filepath.Base(argv[0]), why)
	}
	return tok, nil
}

// findGcloud honours the Python tool's GCLOUD variable, else looks on PATH, then where installers put it: a scheduled job gets a minimal
// PATH, which usually lacks Homebrew.
func findGcloud() string {
	// The Python tool's override names the gcloud to run and is not second-guessed.
	if o := oldnames.Getenv(oldnames.RecorderEnv("GCLOUD")); o != "" {
		if st, err := os.Stat(o); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return o
		}
		return ""
	}
	if p, err := exec.LookPath("gcloud"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{"/opt/homebrew/bin/gcloud", "/usr/local/bin/gcloud", "/usr/bin/gcloud",
		"/opt/homebrew/share/google-cloud-sdk/bin/gcloud", "/usr/lib/google-cloud-sdk/bin/gcloud",
		filepath.Join(home, "google-cloud-sdk", "bin", "gcloud")} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// Ready mints the pass's token, proving the writer identity can be acted as, without
// writing anything.
func (s *Sink) Ready(ctx context.Context) error {
	_, err := s.accessToken(ctx)
	return err
}
