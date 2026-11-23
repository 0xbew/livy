package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Credentials are short-lived AWS keys, held in memory for one pass.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

const commandTimeout = 60 * time.Second

var (
	imdsBase       = "http://169.254.169.254"
	containerHost  = "http://169.254.170.2"
	metadataClient = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Second}
)

// resolve walks the chain, first answer wins:
//  1. AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (and AWS_SESSION_TOKEN) in the environment;
//  2. sink.credential_command, which prints credential_process JSON;
//  3. the aws CLI, `aws configure export-credentials --format process [--profile P]`, which
//     follows the CLI's own chain: SSO, profiles, credential_process, web identity;
//  4. the container credentials endpoint (ECS, EKS Pod Identity);
//  5. the instance role, through IMDSv2.
//
// A profile names step 3 explicitly, so env keys are skipped when one is set.
func resolve(ctx context.Context, c Config) (Credentials, error) {
	if c.Profile == "" {
		if id, secret := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"); id != "" && secret != "" {
			return Credentials{id, secret, os.Getenv("AWS_SESSION_TOKEN")}, nil
		}
	}
	if len(c.CredentialCommand) > 0 {
		return processCredentials(ctx, c.CredentialCommand)
	}
	var cliErr error
	if aws, err := exec.LookPath("aws"); err == nil {
		argv := []string{aws, "configure", "export-credentials", "--format", "process"}
		if c.Profile != "" {
			argv = append(argv, "--profile", c.Profile)
		}
		cr, err := processCredentials(ctx, argv)
		if err == nil || c.Profile != "" {
			return cr, err
		}
		cliErr = err
	} else if c.Profile != "" {
		return Credentials{}, fmt.Errorf("sink.profile %q needs the aws CLI, which is not on PATH", c.Profile)
	}
	if cr, err := containerCredentials(ctx); err == nil {
		return cr, nil
	}
	if cr, err := instanceCredentials(ctx); err == nil {
		return cr, nil
	}
	if cliErr != nil {
		return Credentials{}, fmt.Errorf("%w (the aws CLI said: %v)", errNoCredentials, cliErr)
	}
	return Credentials{}, errNoCredentials
}

// processCredentials runs a credential_process command (no stdin: a login prompt must
// fail, not hang) and reads its JSON.
func processCredentials(ctx context.Context, argv []string) (Credentials, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		return Credentials{}, fmt.Errorf("%s: %s", argv[0], strings.TrimSpace(lines[len(lines)-1]+" "+err.Error()))
	}
	var p struct {
		Version         int
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string
		SessionToken    string
	}
	if err := json.Unmarshal(out, &p); err != nil || p.AccessKeyID == "" || p.SecretAccessKey == "" {
		return Credentials{}, fmt.Errorf("%s did not print credential_process JSON", argv[0])
	}
	return Credentials{p.AccessKeyID, p.SecretAccessKey, p.SessionToken}, nil
}

type roleCredentials struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string
	Token           string
}

func (r roleCredentials) valid(where string) (Credentials, error) {
	if r.AccessKeyID == "" || r.SecretAccessKey == "" {
		return Credentials{}, fmt.Errorf("%s answered without credentials", where)
	}
	return Credentials{r.AccessKeyID, r.SecretAccessKey, r.Token}, nil
}

func containerCredentials(ctx context.Context) (Credentials, error) {
	u := os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI")
	if u == "" {
		rel := os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")
		if rel == "" {
			return Credentials{}, fmt.Errorf("not in a container with a role")
		}
		u = containerHost + rel
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Credentials{}, err
	}
	token := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN")
	if f := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); f != "" {
		if b, err := os.ReadFile(f); err == nil {
			token = strings.TrimSpace(string(b))
		}
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	var r roleCredentials
	if err := getJSON(req, &r); err != nil {
		return Credentials{}, err
	}
	return r.valid("the container credentials endpoint")
}

func instanceCredentials(ctx context.Context) (Credentials, error) {
	tokReq, err := http.NewRequestWithContext(ctx, http.MethodPut, imdsBase+"/latest/api/token", nil)
	if err != nil {
		return Credentials{}, err
	}
	tokReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "300")
	tok, err := getText(tokReq)
	if err != nil {
		return Credentials{}, err
	}
	get := func(path string) (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, imdsBase+path, nil)
		if err == nil {
			r.Header.Set("X-aws-ec2-metadata-token", tok)
		}
		return r, err
	}
	roleReq, err := get("/latest/meta-data/iam/security-credentials/")
	if err != nil {
		return Credentials{}, err
	}
	role, err := getText(roleReq)
	if err != nil || role == "" {
		return Credentials{}, fmt.Errorf("the instance has no role")
	}
	credReq, err := get("/latest/meta-data/iam/security-credentials/" + strings.SplitN(role, "\n", 2)[0])
	if err != nil {
		return Credentials{}, err
	}
	var r roleCredentials
	if err := getJSON(credReq, &r); err != nil {
		return Credentials{}, err
	}
	return r.valid("the instance metadata service")
}

func getText(req *http.Request) (string, error) {
	resp, err := metadataClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return strings.TrimSpace(string(b)), nil
}

func getJSON(req *http.Request, out any) error {
	s, err := getText(req)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(s), out)
}
