package s3

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
)

// maxRead bounds an object read into memory (the denylist): far above any real one.
const maxRead = 8 << 20

// Get reads one object. An object that is not there is fs.ErrNotExist. An error carries
// only S3's error code, never the body: a signature error's body echoes the request.
func (s *Sink) Get(ctx context.Context, key string) ([]byte, error) {
	creds, err := s.credentials(ctx)
	if err != nil {
		return nil, err
	}
	host, path, base := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Host = host
	Sign(req, path, nil, *creds, s.c.Region, "s3", s.now())
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read %s: %v", s.URL(key), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRead))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%s: %w", s.URL(key), fs.ErrNotExist)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("read %s: HTTP %d %s", s.URL(key), resp.StatusCode, errorCode(body))
	case err != nil:
		return nil, fmt.Errorf("read %s: %v", s.URL(key), err)
	}
	return body, nil
}
