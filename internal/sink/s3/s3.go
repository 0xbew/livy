// Package s3 is the Amazon S3 (and S3-compatible) sink: each object is created with
// `If-None-Match: *`, so it never replaces one, and an object already there (HTTP 412) is
// reported as fs.ErrExist. Requests are signed with AWS Signature Version 4, written here on
// the standard library: no AWS SDK.
//
// Credentials come from the standard AWS chain and are never stored: the environment, a
// credential_process command, the aws CLI (which covers SSO and named profiles), the
// container credentials endpoint, then the instance role (IMDSv2). They are fetched once
// per Sink (one per emit pass) and kept in memory only.
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config is the `sink` section for type s3.
//
//	{"sink": {"type": "s3", "bucket": "my-bucket", "prefix": "optional/path",
//	          "region": "us-east-1", "profile": "my-sso-profile",
//	          "credential_command": ["my-cred-process"],
//	          "endpoint": "http://localhost:9000"}}
//
// With an endpoint (an S3-compatible server) objects are addressed path-style
// (endpoint/bucket/key); without one, virtual-hosted on AWS (bucket.s3.region.amazonaws.com).
type Config struct {
	Bucket            string
	Prefix            string
	Region            string
	Endpoint          string
	Profile           string
	CredentialCommand []string
}

// Sink uploads to one bucket.
type Sink struct {
	c      Config
	client *http.Client
	now    func() time.Time
	mu     sync.Mutex
	creds  *Credentials
}

// New checks the config; nothing is called until the first Put.
func New(c Config) (*Sink, error) {
	c.Bucket = strings.TrimSuffix(strings.TrimPrefix(c.Bucket, "s3://"), "/")
	if c.Bucket == "" || strings.Contains(c.Bucket, "/") {
		return nil, fmt.Errorf("sink.bucket must name a bucket, got %q", c.Bucket)
	}
	c.Prefix = strings.Trim(c.Prefix, "/")
	if c.Region == "" {
		c.Region = firstEnv("AWS_REGION", "AWS_DEFAULT_REGION")
	}
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	c.Endpoint = strings.TrimSuffix(c.Endpoint, "/")
	if c.Endpoint != "" {
		if u, err := url.Parse(c.Endpoint); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("sink.endpoint %q is not an http(s) URL", c.Endpoint)
		}
	}
	// Never follow a redirect: Go turns a redirected PUT into a GET, whose 200 would read as
	// "stored". A 3xx (S3's wrong-region answer among them) is a failure and the copy is kept.
	client := &http.Client{Timeout: 2 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Sink{c: c, client: client, now: time.Now}, nil
}

func (s *Sink) name(key string) string {
	if s.c.Prefix == "" {
		return key
	}
	return s.c.Prefix + "/" + key
}

// URL is s3://bucket/[prefix/]key.
func (s *Sink) URL(key string) string { return "s3://" + s.c.Bucket + "/" + s.name(key) }

func (s *Sink) objectURL(key string) (host, path, base string) {
	path = "/" + uriEncode(s.name(key), false)
	if s.c.Endpoint != "" {
		u, _ := url.Parse(s.c.Endpoint)
		prefix := strings.TrimSuffix(u.EscapedPath(), "/")
		return u.Host, prefix + "/" + uriEncode(s.c.Bucket, true) + path, u.Scheme + "://" + u.Host
	}
	if strings.Contains(s.c.Bucket, ".") {
		// A dotted bucket does not match the *.s3 certificate as a host name: path-style.
		host = "s3." + s.c.Region + ".amazonaws.com"
		return host, "/" + uriEncode(s.c.Bucket, true) + path, "https://" + host
	}
	host = s.c.Bucket + ".s3." + s.c.Region + ".amazonaws.com"
	return host, path, "https://" + host
}

// Put creates the object, never replacing one. Only a 2xx with an ETag means it was stored,
// and only a 412 PreconditionFailed means it is already there (its key is the hash of these
// bytes): either lets the caller delete its copy, so anything less certain is a failure.
func (s *Sink) Put(ctx context.Context, key string, body []byte) error {
	creds, err := s.credentials(ctx)
	if err != nil {
		return err
	}
	host, path, base := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Host = host
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("If-None-Match", "*")
	Sign(req, path, body, *creds, s.c.Region, "s3", s.now())
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("upload to %s: %v", s.URL(key), err)
	}
	defer resp.Body.Close()
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300 && resp.Header.Get("ETag") != "":
		return nil
	case resp.StatusCode == http.StatusPreconditionFailed && errorCode(detail) == "PreconditionFailed":
		return fmt.Errorf("%s: %w", s.URL(key), fs.ErrExist)
	default:
		return fmt.Errorf("upload to %s: HTTP %d %s", s.URL(key), resp.StatusCode, errorCode(detail))
	}
}

func (s *Sink) credentials(ctx context.Context) (*Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creds != nil {
		return s.creds, nil
	}
	c, err := resolve(ctx, s.c)
	if err != nil {
		return nil, err
	}
	s.creds = &c
	return s.creds, nil
}

// Sign adds SigV4 headers (x-amz-date, x-amz-content-sha256, x-amz-security-token when
// the credentials carry one, and Authorization) to req. Every header already on req, plus
// host, is signed. path is the request's path exactly as sent (already URI-encoded).
func Sign(req *http.Request, path string, body []byte, c Credentials, region, service string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.Join(strings.Fields(strings.Join(v, ",")), " ")
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{req.Method, path, canonicalQuery(req.URL.Query()),
		canonHeaders.String(), signed, payload}, "\n")
	scope := day + "/" + region + "/" + service + "/aws4_request"
	crSum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(crSum[:])
	key := hmacSHA256([]byte("AWS4"+c.SecretAccessKey), day)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKeyID+"/"+scope+
		", SignedHeaders="+signed+", Signature="+sig)
}

func canonicalQuery(q url.Values) string {
	var pairs [][2]string // sorted by encoded name, then encoded value, as SigV4 requires
	for k, vals := range q {
		for _, v := range vals {
			pairs = append(pairs, [2]string{uriEncode(k, true), uriEncode(v, true)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p[0] + "=" + p[1]
	}
	return strings.Join(parts, "&")
}

// uriEncode is SigV4's URI encoding: everything but A-Z a-z 0-9 - . _ ~ is percent-encoded,
// and '/' too unless it separates path segments.
func uriEncode(s string, slash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && !slash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// errorCode is the <Code> of an S3 <Error> document, never the rest of it: a signature
// error's body echoes the request, credentials' ids among it. "" when it is not one.
func errorCode(b []byte) string {
	var e struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
	}
	if xml.Unmarshal(b, &e) != nil {
		return ""
	}
	if len(e.Code) > 64 {
		return e.Code[:64]
	}
	return e.Code
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

var errNoCredentials = errors.New("no AWS credentials: set AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, " +
	"sink.credential_command, or sink.profile with the aws CLI installed (aws sso login), or run with an instance or container role")

// Ready resolves credentials, proving the chain answers, without writing anything.
func (s *Sink) Ready(ctx context.Context) error {
	_, err := s.credentials(ctx)
	return err
}
