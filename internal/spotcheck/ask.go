package spotcheck

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/0xbew/scrub"

	"github.com/0xbew/livy/internal/oldnames"
)

const (
	// Endpoint is the official one only: look-alike domains that ask for the same key exist.
	Endpoint = "https://api.typesafe.ai/v1/systemone"
	// Model is pinned, not "latest": an unannounced version move changes what the same
	// question answers.
	Model = "jev-1.13.0"

	// callBudget is the whole check's budget, inside the hook timeout install sets (30 s).
	// maxAnswerBytes caps what is read of the endpoint's answer; a verdict is a few hundred bytes.
	maxAnswerBytes = 1 << 20

	callBudget     = 20 * time.Second
	requestTimeout = 8 * time.Second
)

var (
	retryCodes = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true, 529: true}
	authCodes  = map[int]bool{401: true, 403: true}
)

// CheckError is a check that could not be made.
type CheckError struct {
	Reason string
	Auth   bool
}

func (e *CheckError) Error() string { return e.Reason }

// AuditRefusedError is the pre-send audit finding something: nothing was sent. It is not a
// CheckError, so the checks do not pause: the next command is judged on its own.
type AuditRefusedError struct{ Hits []string }

func (e *AuditRefusedError) Error() string { return strings.Join(e.Hits, ", ") }

// Asker is one Jev connection: where it goes and the budgets it keeps.
type Asker struct {
	Endpoint string
	Client   *http.Client
	Budget   time.Duration
	Timeout  time.Duration
}

// NewAsker is the real connection: the official endpoint, the proxy settings of the
// environment, and the CA bundle the Python tool's CA_FILE variable or SSL_CERT_FILE names.
func NewAsker() *Asker {
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if pool := caPool(os.Getenv); pool != nil {
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Asker{Endpoint: Endpoint, Budget: callBudget, Timeout: requestTimeout,
		Client: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}}
}

// caPool is the first bundle that loads, in the order the Python tool tries them; nil means
// the system roots.
func caPool(getenv func(string) string) *x509.CertPool {
	for _, name := range []string{oldnames.RecorderEnv("CA_FILE"), "SSL_CERT_FILE"} {
		if name == "" {
			continue
		}
		path := getenv(name)
		if path == "" {
			continue
		}
		pem, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(pem) {
			return pool
		}
	}
	return nil
}

// Body is the exact request body: Python's json.dumps of the model, the scrubbed state
// and the frozen questions.
func Body(contract string, state obj) []byte {
	return []byte(dumps(obj{{"model", Model}, {"state", state}, {"questions", contracts[contract]}}))
}

// Ask sends one check and returns Jev's reply and the round trip in ms. The exact body is
// audited before anything leaves, and a call is retried only when NO answer arrived (a busy
// code or a failed connection). An answer that arrived but could not be read is not retried:
// the call may already be billed. It is bounded end to end, DNS included.
func (a *Asker) Ask(key, contract string, state obj, d *scrub.Denylist) (map[string]any, int64, error) {
	body := Body(contract, state)
	if hits := scrub.AuditBody(body, d); len(hits) > 0 {
		return nil, 0, &AuditRefusedError{hits}
	}
	type result struct {
		resp map[string]any
		ms   int64
		err  error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- result{err: &CheckError{Reason: fmt.Sprintf("panic: %T", p)}}
			}
		}()
		resp, ms, err := a.call(key, body)
		done <- result{resp, ms, err}
	}()
	select {
	case r := <-done:
		return r.resp, r.ms, r.err
	case <-time.After(a.Budget + 2*time.Second):
		return nil, 0, &CheckError{Reason: "no answer within the budget (hung lookup or connection)"}
	}
}

func (a *Asker) call(key string, body []byte) (map[string]any, int64, error) {
	started := time.Now()
	for attempt := 1; ; attempt++ {
		remaining := a.Budget - time.Since(started)
		if remaining <= time.Second {
			return nil, 0, &CheckError{Reason: fmt.Sprintf("budget spent after %d attempt(s)", attempt-1)}
		}
		resp, reason, err := a.once(key, body, min(a.Timeout, remaining))
		if err != nil {
			return nil, 0, err
		}
		if resp != nil {
			var out map[string]any
			if err := json.Unmarshal(resp, &out); err != nil {
				return nil, 0, &CheckError{Reason: "answer unreadable: JSONDecodeError"}
			}
			if out == nil {
				return nil, 0, &CheckError{Reason: "answer unreadable: not an object"}
			}
			return out, time.Since(started).Milliseconds(), nil
		}
		wait := min(time.Duration(2*attempt)*time.Second, a.Budget-time.Since(started)-time.Second)
		if wait <= 0 {
			return nil, 0, &CheckError{Reason: fmt.Sprintf("%s after %d attempt(s)", reason, attempt)}
		}
		time.Sleep(wait)
	}
}

// once makes one request: the reply bytes, or (nil, why, nil) when no answer arrived and a
// retry is allowed, or a final error.
func (a *Asker) once(key string, body []byte, timeout time.Duration) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	connected := false
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected = true }})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", &CheckError{Reason: "request: " + errName(err)}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.Client.Do(req)
	if err != nil {
		if connected {
			return nil, "", &CheckError{Reason: errName(err) + " before an answer arrived"}
		}
		return nil, "connection: " + errName(err), nil // no connection: nothing was processed
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		reason := fmt.Sprintf("HTTP %d", resp.StatusCode)
		switch {
		case authCodes[resp.StatusCode]:
			return nil, "", &CheckError{Reason: reason + " (key rejected)", Auth: true}
		case retryCodes[resp.StatusCode]:
			return nil, reason, nil
		}
		return nil, "", &CheckError{Reason: reason}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	if err != nil {
		return nil, "", &CheckError{Reason: "answer unreadable: " + errName(err)}
	}
	return data, "", nil
}

// errName is the class name the Python tool's receipts carry for the same fault.
func errName(err error) string {
	var dns *net.DNSError
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var host x509.HostnameError
	var verify *tls.CertificateVerificationError
	switch {
	case errors.As(err, &verify), errors.As(err, &unknown), errors.As(err, &invalid), errors.As(err, &host):
		return "SSLCertVerificationError"
	case errors.As(err, &dns):
		return "gaierror"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "ConnectionRefusedError"
	case errors.Is(err, syscall.ECONNRESET):
		return "ConnectionResetError"
	case errors.Is(err, context.DeadlineExceeded), os.IsTimeout(err):
		return "TimeoutError"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "RemoteDisconnected"
	}
	return "OSError"
}
