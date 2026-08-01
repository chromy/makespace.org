package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func testSite() fstest.MapFS {
	return fstest.MapFS{
		"index.html":       {Data: []byte("<h1>Makespace</h1>")},
		"404.html":         {Data: []byte("<h1>Not found</h1>")},
		"makes/index.html": {Data: []byte("<h1>Makes</h1>")},
		"logo.webp":        {Data: []byte("not really a webp")},
		"css/main.min.45cbc8de152c997753d391dc4cd7a30fa6e2009f44637e8aa0cf2886226cb9d6.css": {Data: []byte("body{}")},
		"8748039701_hu_a5d8507211c6a467.jpg":                                                {Data: []byte("not really a jpeg")},
	}
}

func get(t *testing.T, h http.Handler, method, target string) (*http.Response, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	res := rec.Result()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body of %s %s: %v", method, target, err)
	}
	return res, string(body)
}

func TestServesPages(t *testing.T) {
	h := newHandler(testSite(), nil)

	for _, tc := range []struct{ path, want string }{
		{"/", "<h1>Makespace</h1>"},
		{"/makes/", "<h1>Makes</h1>"},
	} {
		res, body := get(t, h, http.MethodGet, tc.path)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", tc.path, res.StatusCode)
		}
		if body != tc.want {
			t.Errorf("GET %s: body = %q, want %q", tc.path, body, tc.want)
		}
	}
}

// Hugo's pretty URLs mean /makes is a directory and /makes/index.html is the
// file behind it. net/http canonicalises both towards /makes/, with a relative
// Location value, so neither spelling silently serves duplicate content.
func TestRedirectsToCanonicalPath(t *testing.T) {
	h := newHandler(testSite(), nil)

	for _, tc := range []struct{ path, location string }{
		{"/makes", "makes/"},
		{"/index.html", "./"},
		{"/makes/index.html", "./"},
	} {
		res, _ := get(t, h, http.MethodGet, tc.path)
		if res.StatusCode != http.StatusMovedPermanently {
			t.Errorf("GET %s: status = %d, want 301", tc.path, res.StatusCode)
		}
		if got := res.Header.Get("Location"); got != tc.location {
			t.Errorf("GET %s: Location = %q, want %q", tc.path, got, tc.location)
		}
	}
}

func TestCacheControl(t *testing.T) {
	h := newHandler(testSite(), nil)

	for _, tc := range []struct{ path, want string }{
		{"/", "no-cache"},
		{"/makes/", "no-cache"},
		{"/css/main.min.45cbc8de152c997753d391dc4cd7a30fa6e2009f44637e8aa0cf2886226cb9d6.css", "public, max-age=31536000, immutable"},
		{"/8748039701_hu_a5d8507211c6a467.jpg", "public, max-age=31536000, immutable"},
		{"/logo.webp", "public, max-age=3600"},
	} {
		res, _ := get(t, h, http.MethodGet, tc.path)
		if got := res.Header.Get("Cache-Control"); got != tc.want {
			t.Errorf("GET %s: Cache-Control = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestNotFoundServesSitePage(t *testing.T) {
	res, body := get(t, newHandler(testSite(), nil), http.MethodGet, "/nope/")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want html", got)
	}
	if body != "<h1>Not found</h1>" {
		t.Errorf("body = %q, want the site's 404 page", body)
	}
}

// Without a rendered 404.html the wrapper must stay out of the way rather than
// serve an empty page — the site has no 404 template today.
func TestNotFoundWithoutSitePage(t *testing.T) {
	site := testSite()
	delete(site, "404.html")

	res, body := get(t, newHandler(site, nil), http.MethodGet, "/nope/")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
	if body == "" {
		t.Error("body is empty, want net/http's default 404 text")
	}
}

func TestHealthz(t *testing.T) {
	res, _ := get(t, newHandler(testSite(), nil), http.MethodGet, "/healthz")
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
}

func TestRejectsNonGET(t *testing.T) {
	res, _ := get(t, newHandler(testSite(), nil), http.MethodPost, "/")
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", res.StatusCode)
	}
}
