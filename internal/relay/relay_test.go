package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// collector answers each export with the next status in its script and keeps what came in.
type collector struct {
	mu     sync.Mutex
	script []int
	got    []*http.Request
}

func (c *collector) ServeHTTP(w http.ResponseWriter, q *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = io.ReadAll(q.Body)
	c.got = append(c.got, q)
	status := http.StatusOK
	if len(c.got) <= len(c.script) {
		status = c.script[len(c.got)-1]
	}
	w.WriteHeader(status)
}

func start(t *testing.T, script []int, fetches *int) (*collector, *httptest.Server, *Relay) {
	t.Helper()
	col := &collector{script: script}
	up := httptest.NewServer(col)
	t.Cleanup(up.Close)
	r := New(up.URL, "k3y", t.TempDir(), func(context.Context) (map[string]string, error) {
		*fetches++
		return map[string]string{"Authorization": "Bearer token-" + strconv.Itoa(*fetches)}, nil
	})
	r.Report = func(string) {}
	front := httptest.NewServer(nil)
	t.Cleanup(front.Close)
	port, _ := strconv.Atoi(front.URL[strings.LastIndex(front.URL, ":")+1:])
	front.Config.Handler = r.Handler(port)
	return col, front, r
}

func post(t *testing.T, url string, header map[string]string) int {
	t.Helper()
	q, _ := http.NewRequest(http.MethodPost, url+LogsPath, strings.NewReader("\x0a\x00"))
	q.Header.Set("Content-Type", "application/x-protobuf")
	for k, v := range header {
		if k == "Host" {
			q.Host = v
		} else {
			q.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(q)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Guards: another local process, or a web page, exporting as the engineer through the relay.
// Only a request carrying the key, from no browser origin, to this host, is forwarded.
func TestOnlyTheKeyHolderIsForwarded(t *testing.T) {
	fetches := 0
	col, front, _ := start(t, nil, &fetches)
	for _, c := range []struct {
		name   string
		header map[string]string
		want   int
	}{
		{"no key", nil, http.StatusForbidden},
		{"another key", map[string]string{KeyHeader: "k3yy"}, http.StatusForbidden},
		{"a web page", map[string]string{KeyHeader: "k3y", "Origin": "https://evil.example"}, http.StatusForbidden},
		{"another host name", map[string]string{KeyHeader: "k3y", "Host": "evil.example"}, http.StatusForbidden},
		{"Codex", map[string]string{KeyHeader: "k3y"}, http.StatusOK},
	} {
		if got := post(t, front.URL, c.header); got != c.want {
			t.Errorf("%s: HTTP %d, want %d", c.name, got, c.want)
		}
	}
	if len(col.got) != 1 || col.got[0].Header.Get(KeyHeader) != "" || col.got[0].Header.Get("Authorization") != "Bearer token-1" {
		t.Errorf("forwarded %d request(s); want exactly Codex's, with the token and without the relay key", len(col.got))
	}
}

// Guards: the incident the relay exists for. A long-running Codex kept exporting with an
// expired token and every export after the first hour was refused. A refusal must fetch
// fresh headers once and retry; a second refusal stands.
func TestARefusalFetchesFreshHeadersOnce(t *testing.T) {
	fetches := 0
	col, front, _ := start(t, []int{http.StatusUnauthorized, http.StatusOK, http.StatusForbidden, http.StatusForbidden}, &fetches)
	if got := post(t, front.URL, map[string]string{KeyHeader: "k3y"}); got != http.StatusOK {
		t.Errorf("after one refusal: HTTP %d, want 200 with fresh headers", got)
	}
	if got := post(t, front.URL, map[string]string{KeyHeader: "k3y"}); got != http.StatusForbidden {
		t.Errorf("refused twice: HTTP %d, want 403 passed on", got)
	}
	if len(col.got) != 4 || col.got[1].Header.Get("Authorization") != "Bearer token-2" || fetches != 3 {
		t.Errorf("sent %d export(s) with %d header fetch(es); want 4 sends, the retry on fresh headers, 3 fetches", len(col.got), fetches)
	}
}

// Guards: the admin's managed file refused (it may lock otel), and the receiver still sending
// every export on to the collector, or storing it for the sink, as if nothing were wrong.
func TestAHeldReceiverForwardsAndStoresNothing(t *testing.T) {
	fetches := 0
	col, front, r := start(t, nil, &fetches)
	var reasons []string
	r.Held = func() string { return "the managed config file is refused" }
	r.Report = func(reason string) { reasons = append(reasons, reason) }
	for range 2 {
		if got := post(t, front.URL, map[string]string{KeyHeader: "k3y"}); got != http.StatusServiceUnavailable {
			t.Errorf("held: HTTP %d, want 503", got)
		}
	}
	if len(col.got) != 0 {
		t.Errorf("held: forwarded %d export(s), want none", len(col.got))
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "the managed config file is refused") {
		t.Errorf("held: ledger.error reasons %q, want one naming the refused managed file", reasons)
	}
}
