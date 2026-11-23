package s3

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Guards: a request S3 rejects as wrongly signed, which would leave every segment unshipped.
// The expected signature is AWS's published example (S3 SigV4 docs, "Example: GET Object"),
// computed by AWS, not by this code.
func TestSignMatchesAWSPublishedExample(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	creds := Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	Sign(req, "/test.txt", nil, creds, "us-east-1", "s3", time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization\n got %s\nwant %s", got, want)
	}
}

// Guards: a retry replacing an object (If-None-Match is our precondition), and a local
// segment deleted on an answer that does not prove the bucket has it. The answer bodies are
// the shapes S3 returns.
func TestPutIsCreateOnlyAndOnly412IsAlreadyThere(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	status := http.StatusOK
	var ifNoneMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ifNoneMatch = r.Header.Get("If-None-Match")
		if status == http.StatusOK {
			w.Header().Set("ETag", `"abc"`)
		}
		w.WriteHeader(status)
		if status == http.StatusPreconditionFailed {
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>PreconditionFailed</Code></Error>`))
		}
	}))
	defer srv.Close()
	s, err := New(Config{Bucket: "b", Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		status            int
		wantOK, wantExist bool
	}{{http.StatusOK, true, false}, {http.StatusPreconditionFailed, false, true}, {http.StatusForbidden, false, false}} {
		status = c.status
		err := s.Put(context.Background(), "source=agent-hooks/dt=2026-11-03/k.jsonl.gz", []byte("x"))
		if (err == nil) != c.wantOK || errors.Is(err, fs.ErrExist) != c.wantExist {
			t.Errorf("HTTP %d: Put = %v; want ok=%v, already-there=%v", c.status, err, c.wantOK, c.wantExist)
		}
		if ifNoneMatch != "*" {
			t.Errorf("HTTP %d: If-None-Match = %q, want *", c.status, ifNoneMatch)
		}
	}
}
