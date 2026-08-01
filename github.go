package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// PullRequestOpener turns a submission into a pull request and returns its URL.
type PullRequestOpener interface {
	OpenPullRequest(ctx context.Context, path, content, title, body string) (string, error)
}

// githubApp talks to the REST API as a GitHub App installation.
//
// App rather than a personal access token: the credential it holds is a private
// key that mints installation tokens lasting an hour, scoped to this one repo,
// and revocable by uninstalling the app rather than by rotating somebody's
// account. The JWT is assembled here rather than pulled from a library — it is
// two base64 segments and an RS256 signature.
type githubApp struct {
	appID          string
	installationID string
	key            *rsa.PrivateKey
	owner          string
	repo           string
	base           string // branch pull requests target

	api  string // overridable so tests can point at httptest
	http *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

func newGitHubApp(appID, installationID, privateKeyPEM, owner, repo, base string) (*githubApp, error) {
	key, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	return &githubApp{
		appID:          appID,
		installationID: installationID,
		key:            key,
		owner:          owner,
		repo:           repo,
		base:           base,
		api:            "https://api.github.com",
		http:           &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func parsePrivateKey(pemData string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("private key is not PEM — check the secret kept its newlines")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is %T, want RSA", parsed)
	}
	return key, nil
}

// appJWT signs the short-lived assertion that identifies the app itself. GitHub
// rejects anything over ten minutes, and backdates iat to tolerate clock skew.
func (g *githubApp) appJWT(now time.Time) (string, error) {
	header := base64URL([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": g.appID,
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64URL(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, g.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("signing app jwt: %w", err)
	}
	return signing + "." + base64URL(sig), nil
}

func base64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// installationToken returns a cached token, refreshing it a minute before it
// would expire.
func (g *githubApp) installationToken(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token != "" && time.Now().Before(g.tokenExp.Add(-time.Minute)) {
		return g.token, nil
	}

	assertion, err := g.appJWT(time.Now())
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", g.api, g.installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+assertion)
	setGitHubHeaders(req)

	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := doJSON(g.http, req, &out); err != nil {
		return "", fmt.Errorf("minting installation token: %w", err)
	}
	g.token, g.tokenExp = out.Token, out.ExpiresAt
	return g.token, nil
}

// OpenPullRequest branches from base, commits one file, and opens the PR.
// Photos are not committed — they live in the bucket, and the markdown only
// names them.
func (g *githubApp) OpenPullRequest(ctx context.Context, path, content, title, body string) (string, error) {
	token, err := g.installationToken(ctx)
	if err != nil {
		return "", err
	}
	repo := fmt.Sprintf("%s/repos/%s/%s", g.api, g.owner, g.repo)

	var baseRef struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := g.call(ctx, token, http.MethodGet, repo+"/git/ref/heads/"+g.base, nil, &baseRef); err != nil {
		return "", fmt.Errorf("reading %s: %w", g.base, err)
	}

	branch := branchName(path, time.Now())
	if err := g.call(ctx, token, http.MethodPost, repo+"/git/refs", map[string]any{
		"ref": "refs/heads/" + branch,
		"sha": baseRef.Object.SHA,
	}, nil); err != nil {
		return "", fmt.Errorf("creating branch %s: %w", branch, err)
	}

	if err := g.call(ctx, token, http.MethodPut, repo+"/contents/"+path, map[string]any{
		"message": title,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
		"branch":  branch,
	}, nil); err != nil {
		return "", fmt.Errorf("committing %s: %w", path, err)
	}

	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	if err := g.call(ctx, token, http.MethodPost, repo+"/pulls", map[string]any{
		"title": title,
		"head":  branch,
		"base":  g.base,
		"body":  body,
	}, &pr); err != nil {
		return "", fmt.Errorf("opening pull request: %w", err)
	}
	return pr.HTMLURL, nil
}

func (g *githubApp) call(ctx context.Context, token, method, url string, payload, out any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	setGitHubHeaders(req)
	return doJSON(g.http, req, out)
}

func setGitHubHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "makespace.org")
}

func doJSON(client *http.Client, req *http.Request, out any) error {
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		// GitHub puts the useful part in a "message" field; keep it short so it
		// can be logged without dumping a wall of JSON.
		var apiErr struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &apiErr)
		if apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(payload))
		}
		return fmt.Errorf("github returned %d: %s", res.StatusCode, apiErr.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload, out)
}

// branchName keeps the content path readable but adds a timestamp, so two
// submissions with the same title do not collide on the branch. The committed
// file keeps the clean name, since that becomes the page's URL.
func branchName(path string, now time.Time) string {
	stem := strings.TrimSuffix(strings.TrimPrefix(path, "content/"), ".md")
	stem = strings.ReplaceAll(stem, "/", "-")
	return "submit/" + stem + "-" + now.UTC().Format("20060102-150405")
}
