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
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Application Default Credentials, read the way Google's libraries read them, for the
// keyless kinds only:
//   - authorized_user (`gcloud auth application-default login`): a refresh token traded for
//     an access token;
//   - impersonated_service_account (`... login --impersonate-service-account`): the same,
//     then impersonation;
//   - no file at all: the metadata server of the VM or container this runs on.
//
// A service-account key file is refused: Livy never uses a stored key. Workload identity
// federation files (external_account) are not read natively; use auth gcloud or command.

var (
	tokenURL     = "https://oauth2.googleapis.com/token"
	iamURL       = "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/"
	metadataHost = "metadata.google.internal"
)

// Scopes asked of an impersonated token: the sink's writer needs only storage; a token that
// impersonates again (an impersonated ADC file plus writer_sa), or one for Secret Manager
// (PlatformToken), needs cloud-platform.
const (
	storageScope  = "https://www.googleapis.com/auth/devstorage.read_write"
	platformScope = "https://www.googleapis.com/auth/cloud-platform"
)

type adcFile struct {
	Type             string   `json:"type"`
	ClientID         string   `json:"client_id"`
	ClientSecret     string   `json:"client_secret"`
	RefreshToken     string   `json:"refresh_token"`
	ImpersonationURL string   `json:"service_account_impersonation_url"`
	Source           *adcFile `json:"source_credentials"`
}

// adcToken is a token for the ADC identity, then for writerSA (with scope) when one is named.
func adcToken(ctx context.Context, client *http.Client, writerSA, scope string) (string, error) {
	path, explicit := adcPath()
	raw, err := os.ReadFile(path)
	var tok string
	switch {
	case err == nil:
		var f adcFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return "", fmt.Errorf("ADC file %s unreadable: %v", path, err)
		}
		tok, err = fileToken(ctx, client, f, path, writerSA != "", scope)
	case explicit || !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("ADC file %s cannot be read: %v", path, err)
	default:
		tok, err = metadataToken(ctx)
		if err != nil {
			return "", fmt.Errorf("no ADC file at %s and no metadata server (%v); run `gcloud auth application-default login` or set sink.auth", path, err)
		}
	}
	if err != nil {
		return "", err
	}
	if writerSA == "" {
		return tok, nil
	}
	return impersonate(ctx, client, tok, iamURL+url.PathEscape(writerSA)+":generateAccessToken", scope)
}

func adcPath() (string, bool) {
	if p := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); p != "" {
		return p, true
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("APPDATA"), "gcloud", "application_default_credentials.json"), false
	}
	dir := os.Getenv("CLOUDSDK_CONFIG")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config", "gcloud")
	}
	return filepath.Join(dir, "application_default_credentials.json"), false
}

func fileToken(ctx context.Context, client *http.Client, f adcFile, path string, chained bool, scope string) (string, error) {
	switch f.Type {
	case "authorized_user":
		return refresh(ctx, client, f)
	case "impersonated_service_account":
		if f.Source == nil || f.Source.Type != "authorized_user" || f.ImpersonationURL == "" {
			return "", fmt.Errorf("ADC file %s: impersonation from this source is not supported; use sink.auth gcloud", path)
		}
		src, err := refresh(ctx, client, *f.Source)
		if err != nil {
			return "", err
		}
		if chained {
			scope = platformScope
		}
		return impersonate(ctx, client, src, f.ImpersonationURL, scope)
	case "service_account":
		return "", fmt.Errorf("ADC file %s is a service-account key; Livy does not use stored keys", path)
	default:
		return "", fmt.Errorf("ADC file %s has type %q, which is not read natively; use sink.auth gcloud or command", path, f.Type)
	}
}

func refresh(ctx context.Context, client *http.Client, f adcFile) (string, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {f.ClientID},
		"client_secret": {f.ClientSecret}, "refresh_token": {f.RefreshToken}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := doJSON(client, req, &out); err != nil {
		return "", fmt.Errorf("refreshing the ADC login: %w", err)
	}
	return nonEmpty(out.AccessToken, "refreshing the ADC login")
}

func impersonate(ctx context.Context, client *http.Client, source, endpoint, scope string) (string, error) {
	body, _ := json.Marshal(map[string]any{"scope": []string{scope}, "lifetime": "3600s"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+source)
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		AccessToken string `json:"accessToken"`
	}
	if err := doJSON(client, req, &out); err != nil {
		return "", fmt.Errorf("impersonating the writer: %w", err)
	}
	return nonEmpty(out.AccessToken, "impersonating the writer")
}

func metadataToken(ctx context.Context) (string, error) {
	host := metadataHost
	if h := os.Getenv("GCE_METADATA_HOST"); h != "" {
		host = h
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+host+"/computeMetadata/v1/instance/service-accounts/default/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	var out struct {
		AccessToken string `json:"access_token"`
	}
	// The metadata server is link-local: never through a proxy.
	direct := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second}
	if err := doJSON(direct, req, &out); err != nil {
		return "", err
	}
	return nonEmpty(out.AccessToken, "metadata server")
}

func doJSON(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, strings.Join(strings.Fields(string(raw[:min(len(raw), 200)])), " "))
	}
	return json.Unmarshal(raw, out)
}

func nonEmpty(tok, what string) (string, error) {
	if tok == "" {
		return "", errors.New(what + ": no token in the answer")
	}
	return tok, nil
}
