package denylist

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/0xbew/scrub"

	"github.com/0xbew/livy/internal/sink/gcs"
	"github.com/0xbew/livy/internal/sink/s3"
)

// Outcomes of one refresh, as the record and doctor name them (the Python recorder's words).
const (
	Fetched   = "fetched"
	NoVersion = "no_version" // the secret has no version yet: not a failure
	Refused   = "refused"    // a version that is not a valid denylist
	Failed    = "failed"
)

const (
	fetchTimeout          = 60 * time.Second
	secretManagerEndpoint = "https://secretmanager.googleapis.com"
	maxPayload            = 8 << 20
)

// fetched is one fetch's answer. copy is the bytes to install (the copy's own shape) when
// the outcome is Fetched; it is checked again as the reader reads it before it is installed.
type fetched struct {
	outcome string
	version int
	reason  string
	copy    []byte
}

func failed(format string, a ...any) fetched {
	return fetched{outcome: Failed, reason: fmt.Sprintf(format, a...)}
}

func fetch(ctx context.Context, s *Source) fetched {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	switch s.Type {
	case "file":
		b, err := os.ReadFile(s.Path)
		if err != nil {
			return failed("the source file cannot be read (%s)", pathless(err))
		}
		return fetched{outcome: Fetched, copy: b}
	case "gcs":
		sink, err := gcs.New(s.GCS)
		if err != nil {
			return failed("%v", err)
		}
		return object(sink.Get(ctx, s.Object))
	case "s3":
		sink, err := s3.New(s.S3)
		if err != nil {
			return failed("%v", err)
		}
		return object(sink.Get(ctx, s.Object))
	default:
		return secret(ctx, s)
	}
}

func object(b []byte, err error) fetched {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return failed("the object is not there")
	case err != nil:
		return failed("%v", err)
	}
	return fetched{outcome: Fetched, copy: b}
}

// pathless is an OS error without the path it names (a home folder names a person).
func pathless(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// secret reads the latest version of a Secret Manager secret in ONE access call: the answer
// carries the resolved version's name beside its payload, so the number and the content
// always come from the same version, even when one is added mid-fetch. Access is the only
// permission it needs (secretAccessor). With gcloud auth it runs `gcloud secrets versions
// access` as the Python recorder did; otherwise it calls the API with a token minted as
// the sink mints one.
func secret(ctx context.Context, s *Source) fetched {
	auth := s.GCS.Auth
	if auth == "" {
		auth = "adc"
		if s.GCS.Gcloud != "" || gcs.FindGcloud() != "" {
			auth = "gcloud"
		}
	}
	if auth == "gcloud" {
		return gcloudSecret(ctx, s)
	}
	return apiSecret(ctx, s, auth)
}

// gcloudSecret reads the latest version through gcloud, as the configured account and
// writer.
func gcloudSecret(ctx context.Context, s *Source) fetched {
	g := s.GCS.Gcloud
	if g == "" {
		g = gcs.FindGcloud()
	}
	if g == "" {
		return failed("gcloud not found (PATH or the usual install folders); set %s.auth to adc or command", section)
	}
	args := []string{"secrets", "versions", "access", "latest", "--secret=" + s.Secret, "--project=" + s.Project, "--format=json"}
	if s.GCS.Account != "" {
		args = append(args, "--account="+s.GCS.Account)
	}
	if s.GCS.WriterSA != "" {
		args = append(args, "--impersonate-service-account="+s.GCS.WriterSA)
	}
	cmd := exec.CommandContext(ctx, g, args...)
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stderr, cmd.WaitDelay = nil, &stderr, time.Second // no stdin: a re-auth prompt fails, never hangs
	out, err := cmd.Output()
	if err != nil {
		// NOT_FOUND only once the identity may read the secret: before the secret exists,
		// its grant does not either, and the answer is PERMISSION_DENIED.
		if strings.Contains(stderr.String(), "NOT_FOUND") {
			return fetched{outcome: NoVersion, reason: "the secret has no version yet"}
		}
		return failed("%s", gcloudReason(stderr.String(), err))
	}
	return fromAccess(out)
}

// apiSecret reads the latest version from Secret Manager's API with a platform token got by
// auth (adc or command).
func apiSecret(ctx context.Context, s *Source, auth string) fetched {
	c := s.GCS
	c.Auth = auth
	token, err := gcs.PlatformToken(ctx, c, section)
	if err != nil {
		return failed("%v", err)
	}
	base := strings.TrimSuffix(s.GCS.Endpoint, "/")
	if base == "" {
		base = secretManagerEndpoint
	}
	u := base + "/v1/projects/" + url.PathEscape(s.Project) + "/secrets/" + url.PathEscape(s.Secret) + "/versions/latest:access"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return failed("%v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return failed("reading the latest version: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPayload))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fetched{outcome: NoVersion, reason: "the secret has no version yet"}
	case resp.StatusCode != http.StatusOK:
		return failed("reading the latest version: HTTP %d %s", resp.StatusCode, apiStatus(body))
	case err != nil:
		return failed("reading the latest version: %v", err)
	}
	return fromAccess(body)
}

// fromAccess reads an AccessSecretVersionResponse (the API's and `gcloud --format=json`'s):
// the version number from its name, the payload from base64. Fixed messages only: the
// answer holds the payload.
func fromAccess(body []byte) fetched {
	var r struct {
		Name    string `json:"name"`
		Payload struct {
			Data *string `json:"data"`
		} `json:"payload"`
	}
	if json.Unmarshal(body, &r) != nil || r.Payload.Data == nil {
		return failed("the answer did not carry a version name and a payload")
	}
	number := r.Name[strings.LastIndex(r.Name, "/")+1:]
	version, err := strconv.Atoi(number)
	if err != nil || version < 1 || strings.TrimLeft(number, "0123456789") != "" {
		return failed("the answer did not carry a version number and a payload")
	}
	text, err := base64.StdEncoding.DecodeString(*r.Payload.Data)
	if err != nil {
		text, err = base64.URLEncoding.DecodeString(*r.Payload.Data)
	}
	if err != nil {
		return fetched{outcome: Refused, version: version, reason: fmt.Sprintf("v%d is not base64", version)}
	}
	if _, err := scrub.ParseDenylist(text, version); err != nil { // its message never quotes an entry
		return fetched{outcome: Refused, version: version, reason: fmt.Sprintf("v%d is not a valid denylist: %v", version, err)}
	}
	var payload map[string]json.RawMessage
	_ = json.Unmarshal(text, &payload) // ParseDenylist accepted it as an object
	payload["version"] = json.RawMessage(strconv.Itoa(version))
	copyBytes, _ := json.Marshal(payload)
	return fetched{outcome: Fetched, version: version, copy: append(copyBytes, '\n')}
}

// gcloudReason is gcloud's own error line, without the sentence naming the active account.
// Only ever stderr: stdout of an access call is the payload.
func gcloudReason(stderr string, err error) string {
	var lines []string
	for _, l := range strings.Split(stderr, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	line := ""
	for _, l := range lines {
		if strings.HasPrefix(l, "ERROR") {
			line = l
			break
		}
	}
	if line == "" && len(lines) > 0 {
		line = lines[len(lines)-1]
	}
	if line == "" {
		line = err.Error()
	}
	line = strings.SplitN(line, " This command is authenticated as", 2)[0]
	if len(line) > 240 {
		line = line[:240]
	}
	return line
}

// apiStatus is a Google API error's status word (PERMISSION_DENIED), never its message.
func apiStatus(body []byte) string {
	var e struct {
		Error struct {
			Status string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || len(e.Error.Status) > 64 {
		return ""
	}
	return e.Error.Status
}
