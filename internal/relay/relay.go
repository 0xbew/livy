// Package relay is `livy relay`: a local OTLP relay for Codex. Codex exports to 127.0.0.1;
// the relay adds the collector's Authorization header and forwards the bytes unchanged.
//
// Codex reads its export headers once, when it starts, so a header that expires (an hour's
// ID token) or one that must stay out of a file cannot be given to it directly. The relay
// gets its headers by running the same command Claude Code's otelHeadersHelper runs, keeps
// them until near their token's expiry, and fetches them again on a refusal. It runs outside
// every tool loop, kept alive by the scheduler (a launch agent or a systemd user service).
//
// Only what Codex's exporters send is forwarded: POST /v1/logs, /v1/metrics or /v1/traces
// with an OTLP content type,
// addressed to this host, with no Origin header, and carrying the relay key. A web page can
// reach 127.0.0.1 too, and a browser cannot send those headers cross-origin without a
// preflight this never answers. Another account's process can reach the port as well, so
// the key (<data dir>/codex-otel-relay.key, mode 0600, also written into Codex's [otel]
// table) is what makes a request this engineer's.
//
// Never fails silently: a refusal or a failed header fetch answers Codex with the error and
// writes a ledger.error at most once an hour per kind; every outcome is counted in
// <data dir>/codex-otel-relay.json, which doctor reads.
package relay

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/otelignore"
)

const (
	StateFile = "codex-otel-relay.json"
	KeyFile   = "codex-otel-relay.key"
	KeyHeader = "x-livy-relay-key"
	LogsPath  = "/v1/logs"
	// MetricsPath and TracesPath are relayed beside LogsPath, to the same collector.
	MetricsPath = "/v1/metrics"
	TracesPath  = "/v1/traces"

	maxBody         = 32 << 20
	upstreamTimeout = 30 * time.Second
	clientTimeout   = 60 * time.Second
	helperTimeout   = 60 * time.Second
	// Headers are fetched again this long before their token's exp, so one in flight
	// never expires mid-request; headers with no readable exp are kept this long.
	refreshMargin = 10 * time.Minute
	opaqueTTL     = 25 * time.Minute
	reportEvery   = time.Hour
)

// DataNames are the files and folders this package keeps in the data folder, by exact name.
// `livy uninstall --purge` deletes these and nothing else.
func DataNames() []string { return []string{StateFile, KeyFile, OtelDir, DecisionsDir} }

var contentTypes = map[string]bool{"application/x-protobuf": true, "application/json": true}

// State is the relay's outcome counts, read by doctor.
type State struct {
	PID             int    `json:"pid"`
	StartedAt       string `json:"started_at"`
	Upstream        string `json:"upstream"`
	CountingSince   string `json:"counting_since,omitempty"`
	Delivered       int    `json:"delivered"`
	LastDeliveredAt string `json:"last_delivered_at,omitempty"`
	Failed          int    `json:"failed"`
	LastFailedAt    string `json:"last_failed_at,omitempty"`
	LastFailure     string `json:"last_failure,omitempty"`
	Refused         int    `json:"refused"`
	LastRefusedAt   string `json:"last_refused_at,omitempty"`
	// Sink mode: exports stored, and exports dropped by the stream's size cap.
	Dropped       int               `json:"dropped,omitempty"`
	LastDroppedAt string            `json:"last_dropped_at,omitempty"`
	Reported      map[string]string `json:"reported,omitempty"`
	// Sink mode: exports that lost records to otel.ignore, as a listed project's or as naming
	// no project. Never said in a ledger.error, which would ship a trace of the project.
	Ignored       int    `json:"ignored,omitempty"`
	Unattributed  int    `json:"unattributed,omitempty"`
	LastIgnoredAt string `json:"last_ignored_at,omitempty"`
}

// ReadState is the counts file, or a zero State when it is absent or unreadable.
func ReadState(dir string) State {
	var s State
	raw, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err == nil {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// ReadKey is the relay key, or "" when there is none.
func ReadKey(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, KeyFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// Relay holds the header cache, the forwarding and the counts. Safe for concurrent use.
type Relay struct {
	Upstream string
	Key      string
	// Headers returns the headers to add to every forwarded export.
	Headers func(ctx context.Context) (map[string]string, error)
	Dir     string
	Client  *http.Client
	Report  func(reason string) // a ledger.error
	// Store, when set, is sink mode: exports are written to the otel stream, not forwarded.
	Store *Store
	// Held, when set, is asked on every export why OTEL is held off ("" when it is not): a
	// held export is neither forwarded nor stored.
	Held func() string

	hmu     sync.Mutex
	headers map[string]string
	until   time.Time

	smu   sync.Mutex
	state State
}

// New is a relay to collector's /v1/logs, its counts continued from what dir holds.
func New(collector, key, dir string, headers func(context.Context) (map[string]string, error)) *Relay {
	r := &Relay{Upstream: strings.TrimSuffix(collector, "/") + LogsPath, Key: key, Headers: headers, Dir: dir,
		Client: &http.Client{Timeout: upstreamTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		Report: func(reason string) { hook.WriteError(reason, os.Stderr) }}
	r.state = ReadState(dir)
	r.state.PID = os.Getpid()
	r.state.StartedAt = iso(clock.Now())
	r.state.Upstream = r.Upstream
	return r
}

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// tokenExpiry is a bearer JWT's own exp, read without verifying it: only to say when it ends.
func tokenExpiry(h map[string]string) (time.Time, bool) {
	var auth string
	for k, v := range h {
		if http.CanonicalHeaderKey(k) == "Authorization" {
			auth = v
		}
	}
	tok, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		return time.Time{}, false
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return time.Time{}, false
	}
	exp, err := claims.Exp.Int64()
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(exp, 0), true
}

// auth is the cached headers, fetched again when force is set or they are near expiry.
func (r *Relay) auth(ctx context.Context, force bool) (map[string]string, error) {
	r.hmu.Lock()
	defer r.hmu.Unlock()
	now := clock.Now()
	if !force && r.headers != nil && now.Before(r.until) {
		return r.headers, nil
	}
	h, err := r.Headers(ctx)
	if err != nil {
		return nil, err
	}
	if h == nil {
		h = map[string]string{} // `null` is no headers, and is cached like any answer
	}
	r.headers = h
	if exp, ok := tokenExpiry(h); ok {
		r.until = exp.Add(-refreshMargin)
	} else {
		r.until = now.Add(opaqueTTL)
	}
	return h, nil
}

// Forward sends one logs export and returns what to answer Codex with.
func (r *Relay) Forward(ctx context.Context, body []byte, in http.Header) (int, string, []byte) {
	return r.ForwardPath(ctx, LogsPath, body, in)
}

// upstream is the collector URL for one signal's path: Upstream names the logs one.
func (r *Relay) upstream(path string) string {
	return strings.TrimSuffix(r.Upstream, LogsPath) + path
}

// ForwardPath sends one export for the signal at path and returns what to answer Codex with.
func (r *Relay) ForwardPath(ctx context.Context, path string, body []byte, in http.Header) (int, string, []byte) {
	h, err := r.auth(ctx, false)
	if err != nil {
		r.record("no headers: " + err.Error())
		return http.StatusBadGateway, "text/plain", []byte("relay: no Authorization for the collector")
	}
	status, kind, payload, problem := r.send(ctx, path, body, in, h)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// Revoked, or the clock moved: one fresh fetch, then the collector's answer stands.
		if h, err = r.auth(ctx, true); err != nil {
			r.record("no headers: " + err.Error())
			return http.StatusBadGateway, "text/plain", []byte("relay: no Authorization for the collector")
		}
		status, kind, payload, problem = r.send(ctx, path, body, in, h)
	}
	r.record(problem)
	return status, kind, payload
}

func (r *Relay) send(ctx context.Context, path string, body []byte, in http.Header, h map[string]string) (int, string, []byte, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.upstream(path), bytes.NewReader(body))
	if err != nil {
		return http.StatusBadGateway, "text/plain", []byte("relay: bad upstream"), "bad upstream: " + err.Error()
	}
	for _, k := range []string{"Content-Type", "Content-Encoding"} {
		if v := in.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		var ne net.Error
		why := "collector unreachable"
		if errors.As(err, &ne) && ne.Timeout() {
			why = "collector timed out"
		}
		return http.StatusBadGateway, "text/plain", []byte("relay: " + why), why
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	kind := resp.Header.Get("Content-Type")
	switch {
	case err != nil:
		return resp.StatusCode, "text/plain", []byte("relay: the collector's answer broke off"),
			fmt.Sprintf("HTTP %d (answer cut short)", resp.StatusCode)
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return resp.StatusCode, kind, payload, ""
	default:
		return resp.StatusCode, kind, payload, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
}

// record counts one delivery outcome; problem "" is delivered.
func (r *Relay) record(problem string) {
	r.smu.Lock()
	defer r.smu.Unlock()
	now := clock.Now()
	s := &r.state
	if s.CountingSince == "" {
		s.CountingSince = s.StartedAt
	}
	if problem == "" {
		s.Delivered++
		s.LastDeliveredAt = iso(now)
	} else {
		s.Failed++
		s.LastFailedAt = iso(now)
		s.LastFailure = problem
		kind, _, _ := strings.Cut(problem, ":")
		if r.due(kind, now) {
			r.Report(fmt.Sprintf("codex otel relay: %s; Codex telemetry export to %s was not delivered (%d not delivered since %s)",
				problem, r.Upstream, s.Failed, s.CountingSince))
		}
	}
	r.save()
}

// refuse counts a request without this relay's key, apart from delivery failures, which
// any local process could otherwise inflate.
func (r *Relay) refuse(reason string) {
	r.smu.Lock()
	defer r.smu.Unlock()
	now := clock.Now()
	s := &r.state
	if s.CountingSince == "" {
		s.CountingSince = s.StartedAt
	}
	s.Refused++
	s.LastRefusedAt = iso(now)
	if r.due("refused", now) {
		r.Report(fmt.Sprintf("codex otel relay: refused %d request(s) since %s: %s", s.Refused, s.CountingSince, reason))
	}
	r.save()
}

// drop counts an export the size cap refused, apart from the delivery failures.
func (r *Relay) drop(reason string) {
	r.smu.Lock()
	defer r.smu.Unlock()
	now := clock.Now()
	s := &r.state
	if s.CountingSince == "" {
		s.CountingSince = s.StartedAt
	}
	s.Dropped++
	s.LastDroppedAt = iso(now)
	if r.due("dropped", now) {
		r.Report(fmt.Sprintf("otel receiver: %s; %d export(s) dropped since %s", reason, s.Dropped, s.CountingSince))
	}
	r.save()
}

// ignored counts an export that lost records to otel.ignore.
func (r *Relay) ignored(d otelignore.Dropped) {
	r.smu.Lock()
	defer r.smu.Unlock()
	s := &r.state
	if s.CountingSince == "" {
		s.CountingSince = s.StartedAt
	}
	if d.Ignored > 0 {
		s.Ignored++
	}
	if d.Unattributed > 0 {
		s.Unattributed++
	}
	s.LastIgnoredAt = iso(clock.Now())
	r.save()
}

// due says whether a ledger.error of this kind is owed: at most one an hour.
func (r *Relay) due(kind string, now time.Time) bool {
	if r.state.Reported == nil {
		r.state.Reported = map[string]string{}
	}
	if last, err := time.Parse(time.RFC3339, r.state.Reported[kind]); err == nil && now.Sub(last) < reportEvery {
		return false
	}
	r.state.Reported[kind] = iso(now)
	return true
}

func (r *Relay) save() {
	b, _ := json.MarshalIndent(r.state, "", " ")
	path := filepath.Join(r.Dir, StateFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err == nil {
		err = os.Rename(tmp, path)
		if err == nil {
			return
		}
	}
	os.Remove(tmp)
	fmt.Fprintf(os.Stderr, "relay: could not write %s\n", path)
}

// Handler serves the relay on a listener bound to port.
func (r *Relay) Handler(port int) http.Handler {
	hosts := map[string]bool{"127.0.0.1:" + strconv.Itoa(port): true, "localhost:" + strconv.Itoa(port): true}
	return http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		status, why := Admit(q, hosts)
		if status == 0 && (r.Key == "" || subtle.ConstantTimeCompare([]byte(q.Header.Get(KeyHeader)), []byte(r.Key)) != 1) {
			r.refuse("no relay key, or not this relay's: a Codex process started before the key, or another local process (re-run livy install and restart Codex)")
			status, why = http.StatusForbidden, "relay key missing or wrong"
		}
		if status != 0 {
			w.Header().Set("Connection", "close")
			http.Error(w, why, status)
			return
		}
		if r.holds(w) {
			return
		}
		body, err := io.ReadAll(io.LimitReader(q.Body, q.ContentLength))
		if err != nil || int64(len(body)) != q.ContentLength {
			http.Error(w, "body cut short", http.StatusBadRequest)
			return
		}
		var code int
		var kind string
		var payload []byte
		if r.Store != nil {
			code, kind, payload = r.store(q, body)
		} else {
			// Not cancelled when Codex gives up: the collector may already hold the export,
			// and an abort here would read as an outage.
			code, kind, payload = r.ForwardPath(context.WithoutCancel(q.Context()), q.URL.Path, body, q.Header)
		}
		if kind != "" {
			w.Header().Set("Content-Type", kind)
		}
		w.WriteHeader(code)
		_, _ = w.Write(payload)
	})
}

// store writes one export to the otel stream, in sink mode, and counts what came of it.
func (r *Relay) store(q *http.Request, body []byte) (int, string, []byte) {
	code, kind, payload, stored, dropped, problem := r.Store.Write(q.URL.Path, body, q.Header)
	switch {
	case stored:
		r.record("")
		if problem != "" && r.Report != nil {
			r.Report("livy otel receiver: " + problem)
		}
	case dropped:
		r.drop(problem)
	case problem != "":
		r.record(problem)
	}
	return code, kind, payload
}

// holds answers an export while OTEL is held off, counted and said at most once an hour like
// any export not delivered, and is whether it did.
func (r *Relay) holds(w http.ResponseWriter) bool {
	if r.Held == nil {
		return false
	}
	why := r.Held()
	if why == "" {
		return false
	}
	r.record("held: " + why)
	http.Error(w, "livy: OTEL export is held off: "+why, http.StatusServiceUnavailable)
	return true
}

// managedHeld is why every export is held off: a refused managed file may lock otel. It is
// read on each export, so fixing the file needs no restart.
func managedHeld() string {
	res := hook.Settings()
	if !res.ManagedUnusable {
		return ""
	}
	return res.ManagedRefusal()
}

// Admit is the door: 0 when the request is shaped like Codex's own export to this host,
// else the status and reason to refuse it with. The key is checked after this.
func Admit(q *http.Request, hosts map[string]bool) (int, string) {
	switch {
	case q.Method != http.MethodPost:
		return http.StatusNotImplemented, "POST only"
	case q.URL.Path != LogsPath && q.URL.Path != MetricsPath && q.URL.Path != TracesPath:
		return http.StatusNotFound, "only /v1/logs, /v1/metrics and /v1/traces are relayed"
	case q.Header.Get("Origin") != "" || !hosts[q.Host]:
		return http.StatusForbidden, "not from a local exporter"
	}
	kind, _, _ := strings.Cut(q.Header.Get("Content-Type"), ";")
	if !contentTypes[strings.TrimSpace(kind)] {
		return http.StatusUnsupportedMediaType, "OTLP only"
	}
	if q.ContentLength < 0 {
		return http.StatusLengthRequired, "Content-Length required"
	}
	if q.ContentLength > maxBody {
		return http.StatusRequestEntityTooLarge, "too large"
	}
	return 0, ""
}

// CommandHeaders runs command (Claude Code's otelHeadersHelper) through /bin/sh and reads
// the JSON object of headers it prints. "" means no headers.
func CommandHeaders(command, collector string) func(context.Context) (map[string]string, error) {
	return func(ctx context.Context) (map[string]string, error) {
		if command == "" {
			return map[string]string{}, nil
		}
		cctx, cancel := context.WithTimeout(ctx, helperTimeout)
		defer cancel()
		cmd := exec.CommandContext(cctx, "/bin/sh", "-c", command)
		cmd.Env = append(os.Environ(), "OTEL_EXPORTER_OTLP_ENDPOINT="+collector)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		cmd.WaitDelay = time.Second
		out, err := cmd.Output()
		var h map[string]string
		if err != nil || len(bytes.TrimSpace(out)) == 0 {
			lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
			why := lines[len(lines)-1]
			switch {
			case why != "":
			case err != nil:
				why = "the header command failed: " + err.Error()
			default:
				why = "the header command printed nothing"
			}
			return nil, errors.New(why)
		}
		if json.Unmarshal(out, &h) != nil {
			return nil, errors.New("the header command printed something other than a JSON object of header strings")
		}
		return h, nil
	}
}

// Main is `livy relay --collector URL --listen 127.0.0.1:PORT [--headers-command CMD]`.
// It serves until killed; a failure to start is a ledger.error and exit 1, and the
// scheduler starts it again.
func Main(args []string, stderr io.Writer) int {
	var collector, listen, command string
	sink := false
	for i := 0; i < len(args); i++ {
		name, value, has := strings.Cut(args[i], "=")
		if name == "--sink" {
			sink = true
			continue
		}
		if !has && i+1 < len(args) {
			i++
			value = args[i]
		}
		switch name {
		case "--collector":
			collector = value
		case "--listen":
			listen = value
		case "--headers-command":
			command = value
		default:
			fmt.Fprintf(stderr, "relay: unknown argument %q\n", args[i])
			return 2
		}
	}
	host, portText, err := net.SplitHostPort(listen)
	port, perr := strconv.Atoi(portText)
	if (collector == "") == !sink || err != nil || perr != nil || (host != "127.0.0.1" && host != "localhost") {
		fmt.Fprintln(stderr, "usage: livy relay (--collector URL [--headers-command CMD] | --sink) --listen 127.0.0.1:PORT (a loopback address only)")
		return 2
	}
	dir := hook.DataDir()
	key := ReadKey(dir)
	r := New(collector, key, dir, CommandHeaders(command, collector))
	r.Held = managedHeld
	if sink {
		r.Store = &Store{Dir: OtelStreamDir(dir), MaxBytes: DefaultMaxBytes, Ignored: r.ignored}
		r.Upstream = "livy sink: " + r.Store.Dir
		r.state.Upstream = r.Upstream
	}
	if key == "" {
		r.record("no relay key: " + filepath.Join(dir, KeyFile) + " is missing or empty, so nothing tells Codex from another local process (re-run livy install)")
		return 1
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		r.record("cannot listen: " + err.Error())
		return 1
	}
	// Who holds the port, said at once: install waits for this pid before naming the relay.
	r.smu.Lock()
	r.save()
	r.smu.Unlock()
	fmt.Fprintf(stderr, "relay: %s -> %s\n", ln.Addr(), r.Upstream)
	// The bound port, which differs from the one asked for when that was 0.
	srv := &http.Server{Handler: r.Handler(ln.Addr().(*net.TCPAddr).Port), ReadTimeout: clientTimeout,
		ReadHeaderTimeout: clientTimeout}
	if err := srv.Serve(ln); err != nil {
		r.record("stopped serving: " + err.Error())
		return 1
	}
	return 0
}
