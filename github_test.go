package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

// fakeGitHub records the call sequence and answers the four endpoints the PR
// flow uses.
type fakeGitHub struct {
	t                 *testing.T
	calls             []string
	tokenCalls        int
	installationCalls int
	bodies            map[string]map[string]any
	failOn            string
}

func (f *fakeGitHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := r.Method + " " + r.URL.Path
		f.calls = append(f.calls, route)

		if f.bodies == nil {
			f.bodies = map[string]map[string]any{}
		}
		if payload, err := io.ReadAll(r.Body); err == nil && len(payload) > 0 {
			var parsed map[string]any
			if json.Unmarshal(payload, &parsed) == nil {
				f.bodies[route] = parsed
			}
		}

		if f.failOn == route {
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, `{"message":"Reference already exists"}`)
			return
		}

		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			f.installationCalls++
			assertAppJWT(f.t, r.Header.Get("Authorization"))
			json.NewEncoder(w).Encode(map[string]any{"id": 987654})
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			f.tokenCalls++
			assertAppJWT(f.t, r.Header.Get("Authorization"))
			json.NewEncoder(w).Encode(map[string]any{
				"token":      "ghs_installation_token",
				"expires_at": time.Now().Add(time.Hour),
			})
		case strings.Contains(r.URL.Path, "/git/ref/heads/"):
			if got := r.Header.Get("Authorization"); got != "Bearer ghs_installation_token" {
				f.t.Errorf("Authorization = %q, want the installation token", got)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"object": map[string]any{"sha": "basesha123"},
			})
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			json.NewEncoder(w).Encode(map[string]any{
				"html_url": "https://github.com/Makespace/makespace-site/pull/7",
			})
		default:
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	})
}

func assertAppJWT(t *testing.T, header string) {
	t.Helper()
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		t.Fatalf("Authorization = %q, want a Bearer JWT", header)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d segments, want 3", len(parts))
	}
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding claims: %v", err)
	}
	var got struct {
		ISS string `json:"iss"`
		IAT int64  `json:"iat"`
		EXP int64  `json:"exp"`
	}
	if err := json.Unmarshal(claims, &got); err != nil {
		t.Fatalf("parsing claims: %v", err)
	}
	// GitHub takes the client ID as the JWT issuer.
	if got.ISS != "Iv23liQpEXAMPLE" {
		t.Errorf("iss = %q, want the client id", got.ISS)
	}
	if got.IAT > time.Now().Unix() {
		t.Error("iat is in the future; GitHub rejects that on clock skew")
	}
	// GitHub refuses an expiry more than ten minutes out.
	if d := time.Until(time.Unix(got.EXP, 0)); d <= 0 || d > 10*time.Minute {
		t.Errorf("exp is %v away, want between 0 and 10 minutes", d)
	}
}

func testApp(t *testing.T, fake *fakeGitHub) (*githubApp, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	app, err := newGitHubApp("Iv23liQpEXAMPLE", testKeyPEM(t), "Makespace", "makespace-site", "main")
	if err != nil {
		t.Fatal(err)
	}
	app.api = srv.URL
	return app, srv
}

func TestOpenPullRequestCallSequence(t *testing.T) {
	fake := &fakeGitHub{t: t}
	app, _ := testApp(t, fake)

	url, err := app.OpenPullRequest(context.Background(),
		"content/makes/a-shelf.md", "---\ntitle: 'A Shelf'\n---\n", "Add make: A Shelf", "Submitted by Ada L")
	if err != nil {
		t.Fatalf("OpenPullRequest: %v", err)
	}
	if url != "https://github.com/Makespace/makespace-site/pull/7" {
		t.Errorf("url = %q", url)
	}

	want := []string{
		// The installation is discovered from the repo rather than configured.
		"GET /repos/Makespace/makespace-site/installation",
		"POST /app/installations/987654/access_tokens",
		"GET /repos/Makespace/makespace-site/git/ref/heads/main",
		"POST /repos/Makespace/makespace-site/git/refs",
		"PUT /repos/Makespace/makespace-site/contents/content/makes/a-shelf.md",
		"POST /repos/Makespace/makespace-site/pulls",
	}
	if strings.Join(fake.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls =\n%s\nwant\n%s", strings.Join(fake.calls, "\n"), strings.Join(want, "\n"))
	}

	ref, _ := fake.bodies["POST /repos/Makespace/makespace-site/git/refs"]["ref"].(string)
	if !strings.HasPrefix(ref, "refs/heads/submit/makes-a-shelf-") {
		t.Errorf("branch ref = %q, want a timestamped submit/ branch", ref)
	}
	if sha, _ := fake.bodies["POST /repos/Makespace/makespace-site/git/refs"]["sha"].(string); sha != "basesha123" {
		t.Errorf("branch sha = %q, want the base sha", sha)
	}

	commit := fake.bodies["PUT /repos/Makespace/makespace-site/contents/content/makes/a-shelf.md"]
	encoded, _ := commit["content"].(string)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("commit content is not base64: %v", err)
	}
	if !strings.Contains(string(decoded), "title: 'A Shelf'") {
		t.Errorf("commit content = %q", decoded)
	}
	if branch, _ := commit["branch"].(string); !strings.HasPrefix(branch, "submit/makes-a-shelf-") {
		t.Errorf("commit branch = %q, want the new branch", branch)
	}

	pr := fake.bodies["POST /repos/Makespace/makespace-site/pulls"]
	if base, _ := pr["base"].(string); base != "main" {
		t.Errorf("pull request base = %q, want main", base)
	}
}

// The installation token lasts an hour, so a second submission must reuse it
// rather than signing a fresh JWT every time.
func TestInstallationTokenIsCached(t *testing.T) {
	fake := &fakeGitHub{t: t}
	app, _ := testApp(t, fake)

	for range 2 {
		if _, err := app.OpenPullRequest(context.Background(), "content/makes/x.md", "body", "t", "b"); err != nil {
			t.Fatal(err)
		}
	}
	if fake.tokenCalls != 1 {
		t.Errorf("minted %d tokens, want 1", fake.tokenCalls)
	}
	// The installation cannot change without the app being reinstalled, so one
	// lookup per process is enough.
	if fake.installationCalls != 1 {
		t.Errorf("looked up the installation %d times, want 1", fake.installationCalls)
	}
}

// A repo the app is not installed on must fail with something that points at
// the cause rather than at a missing environment variable.
func TestInstallationLookupFailureIsExplained(t *testing.T) {
	fake := &fakeGitHub{t: t, failOn: "GET /repos/Makespace/makespace-site/installation"}
	app, _ := testApp(t, fake)

	_, err := app.OpenPullRequest(context.Background(), "content/makes/x.md", "body", "t", "b")
	if err == nil {
		t.Fatal("OpenPullRequest succeeded without an installation")
	}
	if !strings.Contains(err.Error(), "is the app installed there?") {
		t.Errorf("error = %v, want it to name the likely cause", err)
	}
}

func TestOpenPullRequestSurfacesAPIErrors(t *testing.T) {
	fake := &fakeGitHub{t: t, failOn: "POST /repos/Makespace/makespace-site/git/refs"}
	app, _ := testApp(t, fake)

	_, err := app.OpenPullRequest(context.Background(), "content/makes/x.md", "body", "t", "b")
	if err == nil {
		t.Fatal("OpenPullRequest succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "Reference already exists") {
		t.Errorf("error = %v, want GitHub's message", err)
	}
}

func TestParsePrivateKeyRejectsRubbish(t *testing.T) {
	if _, err := newGitHubApp("Iv23liQpEXAMPLE", "not a pem", "o", "r", "main"); err == nil {
		t.Error("newGitHubApp accepted a non-PEM key")
	}
}

func TestBranchNameIsUniquePerSubmission(t *testing.T) {
	at := time.Date(2026, 8, 1, 12, 30, 15, 0, time.UTC)
	if got, want := branchName("content/makes/a-shelf.md", at), "submit/makes-a-shelf-20260801-123015"; got != want {
		t.Errorf("branchName = %q, want %q", got, want)
	}
}
