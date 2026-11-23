package gcs

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
)

// maxRead bounds an object read into memory (the denylist): far above any real one.
const maxRead = 8 << 20

// Get reads one object. An object that is not there is fs.ErrNotExist. An error never holds
// the token or the object's bytes.
func (s *Sink) Get(ctx context.Context, key string) ([]byte, error) {
	token, err := s.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	u := s.c.Endpoint + "/storage/v1/b/" + url.PathEscape(s.c.Bucket) + "/o/" + url.PathEscape(s.name(key)) + "?alt=media"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
		return nil, fmt.Errorf("read %s: HTTP %d %s", s.URL(key), resp.StatusCode, short(body))
	case err != nil:
		return nil, fmt.Errorf("read %s: %v", s.URL(key), err)
	}
	return body, nil
}

// PlatformToken mints one cloud-platform access token as c names the identity (auth,
// account, writer_sa, token_command), the way the sink mints its own, for another Google API
// (Secret Manager). section names the config keys in errors. Nothing is stored.
func PlatformToken(ctx context.Context, c Config, section string) (string, error) {
	if err := c.settleAuth(section); err != nil {
		return "", err
	}
	s := &Sink{c: c, client: noRedirects(), scope: platformScope}
	return s.accessToken(ctx)
}

// FindGcloud is gcloud's path (PATH, then the usual install folders), or "".
func FindGcloud() string { return findGcloud() }
