// Command server serves the site Hugo renders into public/.
//
// A static host would do this perfectly well today. The reason to run a process
// at all is what comes next: "create a PR to add new content" wants to live on
// the page rather than in a Google Form, and that needs somewhere to put a
// handler. Everything Hugo renders keeps being served as plain files; new
// routes register on the mux alongside the file server.
package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Hugo content-addresses two kinds of file: fingerprinted assets carry a long
// hash before the extension (main.min.<64 hex>.css) and resized images carry
// one after an _hu_ marker (8748039701_hu_<16 hex>.jpg). Either way the name
// changes when the bytes do, so the response can be cached indefinitely.
var fingerprinted = regexp.MustCompile(`(\.|_hu_)[0-9a-f]{16,}\.[a-z0-9]+$`)

func main() {
	addr := flag.String("addr", ":"+envOr("PORT", "8080"), "address to listen on")
	dir := flag.String("dir", envOr("SITE_DIR", "public"), "directory holding the rendered site")
	devMemberName := flag.String("dev-member", "", "accept submissions as this member, for local development only")
	flag.Parse()

	site := os.DirFS(*dir)
	if _, err := fs.Stat(site, "index.html"); err != nil {
		log.Fatalf("no rendered site in %s (run hugo first): %v", *dir, err)
	}

	submit, err := newSubmitHandler(context.Background(), *devMemberName)
	if err != nil {
		log.Fatalf("configuring submissions: %v", err)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           withLogging(newHandler(site, submit)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Fly sends SIGTERM before replacing a machine; finish in-flight responses
	// rather than cutting them off mid-photo.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	log.Printf("serving %s on %s", *dir, *addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// newHandler routes everything Hugo rendered, plus the handlers that are not
// files. A nil submit handler means submissions are not configured, and the
// route says so rather than disappearing.
func newHandler(site fs.FS, submit http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte("ok\n"))
	})
	if submit == nil {
		submit = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Submissions are not configured on this server.", http.StatusServiceUnavailable)
		})
	}
	mux.Handle("POST /submit", submit)
	// A GET pattern answers HEAD too; anything else gets a 405 from the mux.
	mux.Handle("GET /", withCacheControl(withNotFoundPage(site, http.FileServerFS(site))))
	return mux
}

// newSubmitHandler wires the submission route from the environment, and returns
// nil when the GitHub App or bucket configuration is absent — a server with no
// credentials still serves the site perfectly well.
func newSubmitHandler(ctx context.Context, devMemberName string) (http.Handler, error) {
	appID := os.Getenv("GITHUB_APP_ID")
	installationID := os.Getenv("GITHUB_INSTALLATION_ID")
	privateKey := os.Getenv("GITHUB_PRIVATE_KEY")
	bucket := envOr("BUCKET_NAME", "makespace-site-content")
	if appID == "" || installationID == "" || privateKey == "" {
		log.Print("submissions disabled: GITHUB_APP_ID, GITHUB_INSTALLATION_ID and GITHUB_PRIVATE_KEY are not all set")
		return nil, nil
	}

	app, err := newGitHubApp(
		appID, installationID, privateKey,
		envOr("GITHUB_OWNER", "chromy"),
		envOr("GITHUB_REPO", "makespace.org"),
		envOr("GITHUB_BASE_BRANCH", "main"),
	)
	if err != nil {
		return nil, err
	}

	uploader, err := newBucketUploader(ctx, bucket, envOr("AWS_ENDPOINT_URL_S3", "https://fly.storage.tigris.dev"))
	if err != nil {
		return nil, err
	}

	// Identity is the missing piece: without the members' app vouching for a
	// request, deniedAuth turns every submission away.
	var auth Authenticator = deniedAuth{}
	if devMemberName != "" {
		log.Printf("submissions accepting anyone as %q — development only", devMemberName)
		auth = devMember{name: devMemberName}
	}

	return &submitHandler{auth: auth, uploader: uploader, prs: app, now: time.Now}, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func withCacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case fingerprinted.MatchString(path):
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		case strings.HasSuffix(path, "/") || strings.HasSuffix(path, ".html") || strings.HasSuffix(path, ".xml"):
			// Pages and feeds change in place on every deploy, so they have to
			// be revalidated; Last-Modified still keeps the response a 304.
			w.Header().Set("Cache-Control", "no-cache")
		default:
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		next.ServeHTTP(w, r)
	})
}

// withNotFoundPage serves the site's own 404.html, if Hugo rendered one, in
// place of net/http's plain-text default. Without the template it is a no-op.
func withNotFoundPage(site fs.FS, next http.Handler) http.Handler {
	page, err := fs.ReadFile(site, "404.html")
	if err != nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := &notFoundCatcher{ResponseWriter: w}
		next.ServeHTTP(c, r)
		if !c.caught {
			return
		}
		// Nothing was flushed, so the file server's text/plain headers are
		// still ours to overwrite.
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusNotFound)
		w.Write(page)
	})
}

// notFoundCatcher swallows a 404 and its body so the wrapper above can write
// its own response. Every other status passes straight through.
type notFoundCatcher struct {
	http.ResponseWriter
	caught bool
}

func (c *notFoundCatcher) WriteHeader(code int) {
	if code == http.StatusNotFound {
		c.caught = true
		return
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *notFoundCatcher) Write(b []byte) (int, error) {
	if c.caught {
		return len(b), nil
	}
	return c.ResponseWriter.Write(b)
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
