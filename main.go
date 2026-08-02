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
	upload := flag.Bool("upload", false, "upload the named image files to the bucket and print their keys, instead of serving")
	uploadSlug := flag.String("slug", "", "with -upload: the slug of the post the photos belong to")
	flag.Parse()

	if *upload {
		if err := runUpload(context.Background(), *uploadSlug, flag.Args()); err != nil {
			log.Fatalf("upload: %v", err)
		}
		return
	}

	site := os.DirFS(*dir)
	if _, err := fs.Stat(site, "index.html"); err != nil {
		log.Fatalf("no rendered site in %s (run hugo first): %v", *dir, err)
	}

	submit, err := newSubmitHandler(context.Background())
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
// routes say so rather than disappearing.
func newHandler(site fs.FS, submit *submitHandler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte("ok\n"))
	})
	if submit == nil {
		unconfigured := func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Submissions are not configured on this server.", http.StatusServiceUnavailable)
		}
		mux.HandleFunc("POST /submit", unconfigured)
		mux.HandleFunc("POST /submit/photos", unconfigured)
		mux.HandleFunc("POST /submit/writeup", unconfigured)
	} else {
		mux.HandleFunc("POST /submit", submit.Make)
		mux.HandleFunc("POST /submit/photos", submit.Photos)
		mux.HandleFunc("POST /submit/writeup", submit.Writeup)
	}
	// A GET pattern answers HEAD too; anything else gets a 405 from the mux.
	mux.Handle("GET /", withCacheControl(withNotFoundPage(site, http.FileServerFS(site))))
	return mux
}

// newSubmitHandler wires the submission route from the environment, and returns
// nil when the GitHub App or bucket configuration is absent — a server with no
// credentials still serves the site perfectly well.
func newSubmitHandler(ctx context.Context) (*submitHandler, error) {
	clientID := os.Getenv("GITHUB_CLIENT_ID")
	privateKey := os.Getenv("GITHUB_PRIVATE_KEY")
	// The codeword lives in the environment rather than the source because this
	// repository is public — committing it would publish the thing it guards.
	codeword := os.Getenv("SUBMIT_CODEWORD")
	bucket := envOr("BUCKET_NAME", "makespace-site-content")
	if clientID == "" || privateKey == "" || codeword == "" {
		log.Print("submissions disabled: GITHUB_CLIENT_ID, GITHUB_PRIVATE_KEY and SUBMIT_CODEWORD are not all set")
		return nil, nil
	}

	app, err := newGitHubApp(
		clientID, privateKey,
		envOr("GITHUB_OWNER", "Makespace"),
		envOr("GITHUB_REPO", "makespace-site"),
		envOr("GITHUB_BASE_BRANCH", "main"),
	)
	if err != nil {
		return nil, err
	}

	uploader, err := newBucketUploader(ctx, bucket,
		envOr("AWS_ENDPOINT_URL_S3", "https://fly.storage.tigris.dev"),
		envOr("AWS_REGION", "auto"))
	if err != nil {
		return nil, err
	}

	return &submitHandler{codeword: codeword, uploader: uploader, prs: app, now: time.Now}, nil
}

// runUpload builds just enough of the server to put files in the bucket: no
// GitHub App, no codeword, just the bucket credentials.
func runUpload(ctx context.Context, slug string, paths []string) error {
	bucket := envOr("BUCKET_NAME", "makespace-site-content")
	endpoint := envOr("AWS_ENDPOINT_URL_S3", "https://fly.storage.tigris.dev")

	uploader, err := newBucketUploader(ctx, bucket, endpoint, envOr("AWS_REGION", "auto"))
	if err != nil {
		return err
	}
	// Where the site will read them back from, which is the bucket's own
	// virtual-host name rather than the endpoint.
	baseURL := strings.Replace(endpoint, "https://", "https://"+bucket+".", 1)
	return uploadFiles(ctx, uploader, baseURL, slug, paths, os.Stdout)
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
