//go:build unix

package denylist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xbew/scrub"

	"github.com/0xbew/livy/internal/config"
)

// fixture is a Secret Manager AccessSecretVersionResponse in the API's shape (base64
// payload, version only in the resource name), carrying a made-up list at version 7.
func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "access-secret-version.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// asCopy is the fixture's list in the copy's own shape, as a file, gcs or s3 source holds it.
func asCopy(t *testing.T, version int) []byte {
	t.Helper()
	var r struct {
		Payload struct{ Data []byte } `json:"payload"` // encoding/json decodes base64 into []byte
	}
	if err := json.Unmarshal(fixture(t), &r); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(r.Payload.Data, &m); err != nil {
		t.Fatal(err)
	}
	m["version"] = version
	b, _ := json.Marshal(m)
	return b
}

func settings(t *testing.T, source map[string]any, extra map[string]any) config.Resolved {
	t.Helper()
	top := map[string]any{"denylist": map[string]any{"source": source}}
	for k, v := range extra {
		top[k] = v
	}
	b, _ := json.Marshal(top)
	res := config.Load(config.Inputs{User: b})
	if len(res.Refused) > 0 {
		t.Fatal(res.Refused)
	}
	return res
}

// fakeGcloud writes a gcloud that records its arguments and answers like gcloud does.
func fakeGcloud(t *testing.T, dir, stdout, stderr string, code int) (bin, argsFile string) {
	t.Helper()
	argsFile = filepath.Join(dir, "gcloud-args")
	out, errf := filepath.Join(dir, "gcloud-stdout"), filepath.Join(dir, "gcloud-stderr")
	_ = os.WriteFile(out, []byte(stdout), 0o600)
	_ = os.WriteFile(errf, []byte(stderr), 0o600)
	bin = filepath.Join(dir, "gcloud")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\ncat '" + out + "'\ncat '" + errf + "' >&2\nexit " +
		map[bool]string{true: "0", false: "1"}[code == 0] + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

var now = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// Guards: spot-checks (and every later sender) running with no identity list, or a list
// at the wrong version, because the refresh read the version from somewhere other than the
// answer's resource name, failed to decode the base64 payload, or asked gcloud for
// something other than the one `versions access` call a secretAccessor-only writer may make
// (the Python recorder's exact call). Expected values come from the recorded answer.
func TestEverySourceInstallsTheVersionItServes(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-example")
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.EscapedPath()+"?"+r.URL.RawQuery, r.Header.Get("Authorization")
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/"):
			_, _ = w.Write(fixture(t))
		case strings.HasPrefix(r.URL.Path, "/storage/"):
			_, _ = w.Write(asCopy(t, 11))
		default: // s3, path-style
			_, _ = w.Write(asCopy(t, 12))
		}
	}))
	defer srv.Close()
	tok := []string{"printf", "tok-1"}
	cases := []struct {
		name     string
		source   func(dir string) map[string]any
		version  int
		wantPath string
		wantArgs string
	}{
		{"secretmanager-api", func(string) map[string]any {
			return map[string]any{"type": "secretmanager", "secret": "identity-denylist", "project": "example-project",
				"auth": "command", "token_command": tok, "endpoint": srv.URL}
		}, 7, "/v1/projects/example-project/secrets/identity-denylist/versions/latest:access?", ""},
		{"secretmanager-gcloud", func(dir string) map[string]any {
			bin, _ := fakeGcloud(t, dir, string(fixture(t)), "", 0)
			return map[string]any{"type": "secretmanager", "secret": "identity-denylist", "project": "example-project",
				"auth": "gcloud", "gcloud": bin, "account": "dev@example.test",
				"writer_sa": "writer@example-project.iam.gserviceaccount.com"}
		}, 7, "", "secrets\nversions\naccess\nlatest\n--secret=identity-denylist\n--project=example-project\n" +
			"--format=json\n--account=dev@example.test\n--impersonate-service-account=writer@example-project.iam.gserviceaccount.com\n"},
		{"gcs", func(string) map[string]any {
			return map[string]any{"type": "gcs", "bucket": "bkt", "object": "lists/denylist.json", "auth": "command",
				"token_command": tok, "endpoint": srv.URL}
		}, 11, "/storage/v1/b/bkt/o/lists%2Fdenylist.json?alt=media", ""},
		{"s3", func(string) map[string]any {
			return map[string]any{"type": "s3", "bucket": "bkt", "object": "lists/denylist.json", "endpoint": srv.URL}
		}, 12, "/bkt/lists/denylist.json?", ""},
		{"file", func(dir string) map[string]any {
			p := filepath.Join(dir, "managed-denylist.json")
			_ = os.WriteFile(p, asCopy(t, 4), 0o644)
			return map[string]any{"type": "file", "path": p}
		}, 4, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			gotPath, gotAuth = "", ""
			cfg := settings(t, c.source(dir), nil)
			res := Refresh(context.Background(), cfg, dir, now)
			if !res.OK {
				t.Fatalf("refresh not ok: %s", res.Line)
			}
			if status, detail := Check(cfg, dir, now); status != StatusOK || (c.name != "file" && !strings.Contains(detail, "confirmed against")) {
				t.Errorf("doctor after a good refresh: %s %q; want ok and confirmed", status, detail)
			}
			d, err := scrub.LoadDenylist(filepath.Join(dir, FileName))
			if err != nil || d.Version != c.version {
				t.Fatalf("copy = %v, %v; want version %d", d, err, c.version)
			}
			if st, _ := os.Stat(filepath.Join(dir, FileName)); st.Mode().Perm() != 0o600 {
				t.Errorf("copy mode %04o; want 0600", st.Mode().Perm())
			}
			rec, ok := ReadRecord(dir)
			if !ok || rec.Outcome != Fetched || rec.Version == nil || *rec.Version != c.version {
				t.Errorf("record = %+v; want fetched v%d", rec, c.version)
			}
			if c.wantPath != "" && (gotPath != c.wantPath || (c.name != "s3" && gotAuth != "Bearer tok-1")) {
				t.Errorf("request %q (auth %q); want %q with the minted token", gotPath, gotAuth, c.wantPath)
			}
			if c.wantArgs != "" {
				b, _ := os.ReadFile(filepath.Join(dir, "gcloud-args"))
				if string(b) != c.wantArgs {
					t.Errorf("gcloud args:\n%s\nwant:\n%s", b, c.wantArgs)
				}
			}
		})
	}
}

// Guards: a refresh that fails, finds nothing, or is handed something that is not a whole
// valid denylist replacing (or deleting) the good copy every sender scrubs with, and doing so
// without a word: the copy must be byte-for-byte what it was, the record must say why, and
// the record and line must never carry gcloud's sentence naming the signed-in account.
func TestARefreshThatCannotInstallKeepsTheCopy(t *testing.T) {
	notJSON := `{"name":"projects/1/secrets/s/versions/8","payload":{"data":"bm90IGpzb24="}}`                           // "not json"
	noIDs := `{"name":"projects/1/secrets/s/versions/9","payload":{"data":"eyJpZGVudGl0aWVzIjpbXSwidmVuZG9ycyI6W119"}}` // empty lists
	status := http.StatusOK
	body := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	api := func() map[string]any {
		return map[string]any{"type": "secretmanager", "secret": "s", "project": "p", "auth": "command",
			"token_command": []string{"printf", "tok"}, "endpoint": srv.URL}
	}
	gcsSrc := func() map[string]any {
		return map[string]any{"type": "gcs", "bucket": "b", "object": "d.json", "auth": "command",
			"token_command": []string{"printf", "tok"}, "endpoint": srv.URL}
	}
	cases := []struct {
		name        string
		status      int
		body        string
		source      func(dir string) map[string]any
		wantOutcome string
		wantOK      bool
	}{
		{"api-500", 500, `{"error":{"code":500,"status":"INTERNAL"}}`, func(string) map[string]any { return api() }, Failed, false},
		{"api-no-version", 404, `{"error":{"code":404,"status":"NOT_FOUND"}}`, func(string) map[string]any { return api() }, NoVersion, true},
		{"api-not-json", 200, notJSON, func(string) map[string]any { return api() }, Refused, false},
		{"api-no-identities", 200, noIDs, func(string) map[string]any { return api() }, Refused, false},
		{"api-empty-answer", 200, "", func(string) map[string]any { return api() }, Failed, false},
		{"gcloud-denied", 0, "", func(dir string) map[string]any {
			bin, _ := fakeGcloud(t, dir, "", "ERROR: (gcloud.secrets.versions.access) PERMISSION_DENIED: denied. "+
				"This command is authenticated as dev@example.test which is the active account", 1)
			return map[string]any{"type": "secretmanager", "secret": "s", "project": "p", "auth": "gcloud", "gcloud": bin}
		}, Failed, false},
		{"gcloud-names-the-account", 0, "", func(dir string) map[string]any {
			bin, _ := fakeGcloud(t, dir, "", "ERROR: (gcloud.secrets.versions.access) Your current active account "+
				"[dev@example.test] does not have any valid credentials", 1)
			return map[string]any{"type": "secretmanager", "secret": "s", "project": "p", "auth": "gcloud", "gcloud": bin}
		}, Failed, false},
		{"gcloud-prints-nothing", 0, "", func(dir string) map[string]any { // the conformance fake gcloud's answer
			bin, _ := fakeGcloud(t, dir, "", "", 0)
			return map[string]any{"type": "secretmanager", "secret": "s", "project": "p", "auth": "gcloud", "gcloud": bin}
		}, Failed, false},
		{"gcs-missing-object", 404, `{"error":{"code":404}}`, func(string) map[string]any { return gcsSrc() }, Failed, false},
		{"gcs-object-without-version", 200, `{"identities":[{"literal":"Ada Example","role":"owner"}],"vendors":[]}`,
			func(string) map[string]any { return gcsSrc() }, Refused, false},
		{"file-missing", 0, "", func(dir string) map[string]any {
			return map[string]any{"type": "file", "path": filepath.Join(dir, "nope.json")}
		}, Failed, false},
		{"bad-type", 0, "", func(string) map[string]any { return map[string]any{"type": "vault"} }, Failed, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			good := asCopy(t, 3)
			copyPath := filepath.Join(dir, FileName)
			if err := os.WriteFile(copyPath, good, 0o600); err != nil {
				t.Fatal(err)
			}
			status, body = c.status, c.body
			res := Refresh(context.Background(), settings(t, c.source(dir), nil), dir, now)
			if after, _ := os.ReadFile(copyPath); string(after) != string(good) {
				t.Fatalf("the copy changed: %q", after)
			}
			rec, ok := ReadRecord(dir)
			if res.OK != c.wantOK || !ok || rec.Outcome != c.wantOutcome || rec.Version == nil || *rec.Version != 3 {
				t.Fatalf("ok=%v record=%+v (%s); want ok=%v, %s keeping v3", res.OK, rec, res.Line, c.wantOK, c.wantOutcome)
			}
			if !strings.Contains(res.Line, "kept v3") {
				t.Errorf("line %q does not say the copy was kept", res.Line)
			}
			if c.wantOutcome != NoVersion && (rec.Reason == nil || *rec.Reason == "") {
				t.Errorf("no reason recorded")
			}
			raw, _ := os.ReadFile(filepath.Join(dir, StateName))
			for _, leak := range []string{"authenticated as", "dev@example.test", "Ada Example", "tok"} {
				if strings.Contains(string(raw), leak) || strings.Contains(res.Line, leak) {
					t.Errorf("record or line carries %q: %s / %s", leak, raw, res.Line)
				}
			}
		})
	}
}

// Guards: doctor calling a stopped refresh, a refused version, a world-readable copy, or a
// copy nobody confirmed (touched, copied from elsewhere, or confirmed against another
// source) "ok", or failing a machine that runs with no list (capture works; names go
// unscrubbed). Each state is set up on disk the way the refresh and a person leave it.
func TestDoctorRowStates(t *testing.T) {
	remote := map[string]any{"type": "gcs", "bucket": "b", "object": "d.json", "auth": "command", "token_command": []string{"true"}}
	failedRec := `{"checked_at":"2026-11-09T04:00:00Z","outcome":"failed","version":3,"reason":"HTTP 403 PERMISSION_DENIED","source":"gcs"}`
	sum := sha256.Sum256(asCopy(t, 3))
	hash := hex.EncodeToString(sum[:])
	src, err := FromSettings(settings(t, remote, nil))
	if err != nil {
		t.Fatal(err)
	}
	// okRec is a record whose last successful refresh was ago before now, of the given source
	// and copy; the newest attempt (outcome) is separate from it.
	okRec := func(ago time.Duration, source, sha, outcome string) string {
		at := now.Add(-ago).Format(time.RFC3339)
		return `{"checked_at":"` + at + `","outcome":"` + outcome + `","version":3,"source":"gcs","last_ok_at":"` + at +
			`","last_ok_source":"` + source + `","last_ok_sha256":"` + sha + `"}`
	}
	good := func(ago time.Duration) string { return okRec(ago, sourceID(src), hash, "fetched") }
	cases := []struct {
		name       string
		source     map[string]any
		copy       string
		mode       os.FileMode
		age        time.Duration // the copy's mtime: not evidence of a refresh
		record     string
		wantStatus string
		wantText   []string
	}{
		{"no-copy-no-source", nil, "", 0, 0, "", StatusWarn, []string{"names are not scrubbed", "no denylist.source"}},
		{"no-copy-refresh-failed", remote, "", 0, 0, failedRec, StatusWarn, []string{"names are not scrubbed", "FAILED", "PERMISSION_DENIED"}},
		{"no-copy-never-refreshed", remote, "", 0, 0, "", StatusWarn, []string{"no refresh has run"}},
		{"unusable-copy", remote, "{bad", 0o600, 0, "", StatusFail, []string{"unusable", "send nothing"}},
		{"fresh-remote", remote, "good", 0o600, 90 * 24 * time.Hour, good(5 * time.Hour), StatusOK, []string{"v3", "confirmed against the gcs source 5h ago"}},
		{"stale-remote", remote, "good", 0o600, time.Minute, good(4 * 24 * time.Hour), StatusWarn, []string{"not confirmed for 4d"}},
		{"good-copy-last-refresh-failed", remote, "good", 0o600, time.Hour, okRec(30*time.Hour, sourceID(src), hash, "failed"), StatusWarn, []string{"v3", "confirmed against the gcs source 30h ago"}},
		{"world-readable", remote, "good", 0o644, time.Hour, good(time.Hour), StatusWarn, []string{"mode 0644"}},
		{"hand-kept-old-copy", nil, "good", 0o600, 90 * 24 * time.Hour, "", StatusOK, []string{"v3, kept by hand"}},
		{"file-source-old-copy", map[string]any{"type": "file", "path": "/abs/d.json"}, "good", 0o600, 90 * 24 * time.Hour, "",
			StatusOK, []string{"v3, copied from a local file"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, FileName)
			switch c.copy {
			case "good":
				_ = os.WriteFile(p, asCopy(t, 3), 0o600)
			case "":
			default:
				_ = os.WriteFile(p, []byte(c.copy), 0o600)
			}
			if c.copy != "" {
				_ = os.Chmod(p, c.mode)
				_ = os.Chtimes(p, now.Add(-c.age), now.Add(-c.age))
			}
			if c.record != "" {
				_ = os.WriteFile(filepath.Join(dir, StateName), []byte(c.record), 0o600)
			}
			var res config.Resolved
			if c.source != nil {
				res = settings(t, c.source, nil)
			} else {
				res = config.Load(config.Inputs{})
			}
			status, detail := Check(res, dir, now)
			if status != c.wantStatus {
				t.Errorf("status %q (%s); want %q", status, detail, c.wantStatus)
			}
			for _, w := range c.wantText {
				if !strings.Contains(detail, w) {
					t.Errorf("detail %q lacks %q", detail, w)
				}
			}
		})
	}
}

// Guards: doctor reading a denylist copy as confirmed from its mtime, which a touch, a copy
// from elsewhere or a changed source keeps fresh, when the refresh never confirmed it.
func TestDoctorConfirmationComesFromTheRefreshRecord(t *testing.T) {
	remote := map[string]any{"type": "gcs", "bucket": "b", "object": "d.json", "auth": "command", "token_command": []string{"true"}}
	sum := sha256.Sum256(asCopy(t, 3))
	hash := hex.EncodeToString(sum[:])
	src, err := FromSettings(settings(t, remote, nil))
	if err != nil {
		t.Fatal(err)
	}
	rec := func(ago time.Duration, source, sha string) string {
		at := now.Add(-ago).Format(time.RFC3339)
		return `{"checked_at":"` + at + `","outcome":"fetched","version":3,"source":"gcs","last_ok_at":"` + at +
			`","last_ok_source":"` + source + `","last_ok_sha256":"` + sha + `"}`
	}
	for _, c := range []struct {
		name, record, wantStatus string
		mtimeAge                 time.Duration
		wantText                 string
	}{
		{"old mtime, confirmed 5h ago", rec(5*time.Hour, sourceID(src), hash), StatusOK, 90 * 24 * time.Hour, "confirmed against the gcs source 5h ago"},
		{"fresh mtime, confirmed 4d ago", rec(4*24*time.Hour, sourceID(src), hash), StatusWarn, time.Minute, "not confirmed for 4d"},
		{"touched copy, never confirmed", "", StatusWarn, time.Minute, "no successful refresh is recorded"},
		{"copy replaced since", rec(time.Hour, sourceID(src), strings.Repeat("0", 64)), StatusWarn, time.Minute, "differs from the copy last confirmed"},
		{"source changed since", rec(time.Hour, "gcs:0000000000", hash), StatusWarn, time.Minute, "another source"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, FileName)
			_ = os.WriteFile(p, asCopy(t, 3), 0o600)
			_ = os.Chtimes(p, now.Add(-c.mtimeAge), now.Add(-c.mtimeAge))
			if c.record != "" {
				_ = os.WriteFile(filepath.Join(dir, StateName), []byte(c.record), 0o600)
			}
			status, detail := Check(settings(t, remote, nil), dir, now)
			if status != c.wantStatus || !strings.Contains(detail, c.wantText) {
				t.Errorf("%s %q; want %s containing %q", status, detail, c.wantStatus, c.wantText)
			}
		})
	}
}
