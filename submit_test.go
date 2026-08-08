package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

type fakeUploader struct {
	uploaded map[string][]byte
	types    map[string]string
	err      error
}

func newFakeUploader() *fakeUploader {
	return &fakeUploader{uploaded: map[string][]byte{}, types: map[string]string{}}
}

func (f *fakeUploader) Upload(_ context.Context, key, contentType string, body []byte) error {
	if f.err != nil {
		return f.err
	}
	f.uploaded[key] = body
	f.types[key] = contentType
	return nil
}

type fakePRs struct {
	path, content, title, body string
	calls                      int
	err                        error
}

func (f *fakePRs) OpenPullRequest(_ context.Context, path, content, title, body string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	f.path, f.content, f.title, f.body = path, content, title, body
	return "https://github.com/Makespace/makespace-site/pull/7", nil
}

const testCodeword = "opensesame"

func testHandler() (*submitHandler, *fakeUploader, *fakePRs) {
	up, prs := newFakeUploader(), &fakePRs{}
	h := &submitHandler{
		codeword: testCodeword,
		uploader: up,
		prs:      prs,
		now:      func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) },
	}
	return h, up, prs
}

func formRequest(t *testing.T, fields map[string]string, photos map[string][]byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range photos {
		part, err := w.CreateFormFile("photos", name)
		if err != nil {
			t.Fatal(err)
		}
		part.Write(data)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/submit", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	return req
}

func samplePhoto(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, landscape(), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSubmitOpensPullRequest(t *testing.T) {
	h, up, prs := testHandler()
	req := formRequest(t,
		map[string]string{
			"codeword": testCodeword,
			"name":     "Ada L",
			"license":  "CC-BY-SA-4.0",
			"title":    "A Very Nice Shelf",
			"body":     "Made from offcuts.",
		},
		map[string][]byte{"shelf.jpg": samplePhoto(t)})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var got struct{ Message, URL string }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.URL != "https://github.com/Makespace/makespace-site/pull/7" {
		t.Errorf("URL = %q, want the pull request", got.URL)
	}

	if len(up.uploaded) != 1 {
		t.Fatalf("uploaded %d photos, want 1", len(up.uploaded))
	}
	var key string
	for k := range up.uploaded {
		key = k
	}
	// slug-XXX-sha256.ext, with the slug taken from the title.
	if !regexp.MustCompile(`^a-very-nice-shelf-001-[0-9a-f]{64}\.jpg$`).MatchString(key) {
		t.Errorf("key = %q, want a-very-nice-shelf-001-<sha256>.jpg", key)
	}
	if up.types[key] != "image/jpeg" {
		t.Errorf("content type = %q, want image/jpeg", up.types[key])
	}

	// Named for the day it was submitted, so the directory reads in order.
	if prs.path != "content/makes/2026-08-01-a-very-nice-shelf.md" {
		t.Errorf("path = %q, want the dated content path", prs.path)
	}
	for _, want := range []string{
		"title: 'A Very Nice Shelf'",
		// The slug keeps the posting date out of the URL.
		"slug: 'a-very-nice-shelf'",
		"date: '2026-08-01T12:00:00Z'",
		"draft: false",
		"- 'Ada L'",
		"    license: 'CC-BY-SA-4.0'",
		"        - '" + key + "'",
		"Made from offcuts.",
	} {
		if !strings.Contains(prs.content, want) {
			t.Errorf("markdown missing %q:\n%s", want, prs.content)
		}
	}
}

// The bucket is public and the pull request costs API calls, so a submission
// without the codeword must cost neither.
func TestSubmitRequiresTheCodeword(t *testing.T) {
	for _, tc := range []struct{ name, codeword string }{
		{"missing", ""},
		{"wrong", "notthecodeword"},
		{"different case", strings.ToUpper(testCodeword)},
		{"trailing space", testCodeword + " "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, up, prs := testHandler()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, formRequest(t,
				map[string]string{"codeword": tc.codeword, "name": "Sneaky", "title": "Spam"},
				map[string][]byte{"x.jpg": samplePhoto(t)}))

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
			if len(up.uploaded) != 0 {
				t.Error("a submission without the codeword uploaded a photo")
			}
			if prs.calls != 0 {
				t.Error("a submission without the codeword opened a pull request")
			}
		})
	}
}

func TestSubmitValidation(t *testing.T) {
	photo := samplePhoto(t)
	// Every case carries a valid codeword, so what is being tested is the field
	// validation rather than the gate in front of it.
	fields := func(extra map[string]string) map[string]string {
		f := map[string]string{"codeword": testCodeword, "name": "Ada L", "title": "Fine", "license": "CC-BY-SA-4.0"}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}
	for _, tc := range []struct {
		name   string
		fields map[string]string
		photos map[string][]byte
	}{
		{"no licence", fields(map[string]string{"license": ""}), map[string][]byte{"a.jpg": photo}},
		{"licence not on the list", fields(map[string]string{"license": "wtfpl-2.0"}), map[string][]byte{"a.jpg": photo}},
		{"licence with markup", fields(map[string]string{"license": "CC-BY-4.0'\ninjected: yes"}), map[string][]byte{"a.jpg": photo}},
		{"no name", fields(map[string]string{"name": ""}), map[string][]byte{"a.jpg": photo}},
		{"name too long", fields(map[string]string{"name": strings.Repeat("n", maxNameRunes+1)}), map[string][]byte{"a.jpg": photo}},
		{"no title", fields(map[string]string{"title": ""}), map[string][]byte{"a.jpg": photo}},
		{"no photos", fields(nil), nil},
		{"title with no letters", fields(map[string]string{"title": "!!!"}), map[string][]byte{"a.jpg": photo}},
		{"title too long", fields(map[string]string{"title": strings.Repeat("a", maxTitleRunes+1)}), map[string][]byte{"a.jpg": photo}},
		{"body too long", fields(map[string]string{"body": strings.Repeat("b", maxBodyRunes+1)}), map[string][]byte{"a.jpg": photo}},
		{"not an image", fields(nil), map[string][]byte{"a.jpg": []byte("nope")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, prs := testHandler()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, formRequest(t, tc.fields, tc.photos))

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if prs.calls != 0 {
				t.Error("a rejected submission still opened a pull request")
			}
		})
	}
}

// A photo that cannot be uploaded must stop the submission rather than produce
// a pull request referencing a photo that is not there — and must not be
// reported as a problem with the member's file, which it is not.
func TestSubmitStopsWhenUploadFails(t *testing.T) {
	h, up, prs := testHandler()
	up.err = context.DeadlineExceeded

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, formRequest(t,
		map[string]string{"codeword": testCodeword, "name": "Ada L", "title": "Shelf", "license": "CC-BY-SA-4.0"},
		map[string][]byte{"a.jpg": samplePhoto(t)}))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 — the photo was fine, the bucket was not", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Could not accept") {
		t.Errorf("blamed the member's file for a storage failure: %s", rec.Body)
	}
	if prs.calls != 0 {
		t.Error("opened a pull request despite the upload failing")
	}
}

// The photos form asks for neither a title nor a date: the post is named for
// the day it was posted, and a hash of its first photo keeps two sets from the
// same day apart.
func TestSubmitPhotosOpensPullRequest(t *testing.T) {
	h, up, prs := testHandler()
	req := formRequest(t,
		map[string]string{
			"codeword": testCodeword,
			"name":     "Ada L",
			"license":  "CC-BY-4.0",
			"body":     "A busy Tuesday.",
		},
		map[string][]byte{"one.jpg": samplePhoto(t)})

	rec := httptest.NewRecorder()
	h.Photos(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var key string
	for k := range up.uploaded {
		key = k
	}
	if !regexp.MustCompile(`^content/photos/2026-08-01-photos-[0-9a-f]{8}\.md$`).MatchString(prs.path) {
		t.Errorf("path = %q, want content/photos/<today>-photos-<hash>.md", prs.path)
	}
	if !regexp.MustCompile(`^photos-[0-9a-f]{8}-001-[0-9a-f]{64}\.jpg$`).MatchString(key) {
		t.Errorf("key = %q, want slug-001-<sha256>.jpg", key)
	}
	for _, want := range []string{
		"title: '1 August 2026'",
		"date: '2026-08-01T12:00:00Z'",
		"- 'Ada L'",
		"    license: 'CC-BY-4.0'",
		"        - '" + key + "'",
		"A busy Tuesday.",
	} {
		if !strings.Contains(prs.content, want) {
			t.Errorf("markdown missing %q:\n%s", want, prs.content)
		}
	}
	if !strings.HasPrefix(prs.title, "Add photos: ") {
		t.Errorf("pull request title = %q, want it to say photos", prs.title)
	}
}

// A writeup is the one kind where the words are the point: the body is required
// and photos are not.
func TestSubmitWriteup(t *testing.T) {
	base := map[string]string{
		"codeword": testCodeword,
		"name":     "Ada L",
		"license":  "CC-BY-SA-4.0",
		"title":    "Bike Maintenance Workshop",
		"body":     "Eight people came. We fixed six bikes and broke one.",
	}

	t.Run("no photos needed", func(t *testing.T) {
		h, up, prs := testHandler()
		rec := httptest.NewRecorder()
		h.Writeup(rec, formRequest(t, base, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
		if len(up.uploaded) != 0 {
			t.Errorf("uploaded %v, want nothing", keysOf(up))
		}
		if prs.path != "content/writeups/2026-08-01-bike-maintenance-workshop.md" {
			t.Errorf("path = %q, want the dated writeups path", prs.path)
		}
		if !strings.HasPrefix(prs.title, "Add writeup: ") {
			t.Errorf("pull request title = %q, want it to say writeup", prs.title)
		}
		// An empty images list would make the templates loop over nothing.
		if strings.Contains(prs.content, "images:") {
			t.Errorf("front matter has an images list with no photos:\n%s", prs.content)
		}
		for _, want := range []string{
			"title: 'Bike Maintenance Workshop'",
			"slug: 'bike-maintenance-workshop'",
			"    license: 'CC-BY-SA-4.0'",
			"We fixed six bikes",
		} {
			if !strings.Contains(prs.content, want) {
				t.Errorf("markdown missing %q:\n%s", want, prs.content)
			}
		}
	})

	t.Run("photos still allowed", func(t *testing.T) {
		h, up, prs := testHandler()
		rec := httptest.NewRecorder()
		h.Writeup(rec, formRequest(t, base, map[string][]byte{"one.jpg": samplePhoto(t)}))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
		for key := range up.uploaded {
			if !strings.HasPrefix(key, "bike-maintenance-workshop-001-") {
				t.Errorf("photo key = %q, want the writeup's slug", key)
			}
		}
		if !strings.Contains(prs.content, "images:") {
			t.Errorf("front matter is missing the photo:\n%s", prs.content)
		}
	})

	t.Run("body is required", func(t *testing.T) {
		fields := map[string]string{}
		for k, v := range base {
			fields[k] = v
		}
		fields["body"] = ""

		h, _, prs := testHandler()
		rec := httptest.NewRecorder()
		h.Writeup(rec, formRequest(t, fields, nil))

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
		if prs.calls != 0 {
			t.Error("an empty writeup opened a pull request")
		}
	})

	t.Run("a make with no photos is still refused", func(t *testing.T) {
		h, _, prs := testHandler()
		rec := httptest.NewRecorder()
		h.Make(rec, formRequest(t, base, nil))

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
		if prs.calls != 0 {
			t.Error("a photoless make opened a pull request")
		}
	})
}

// The slug is optional on both forms. Given one it names the file, the URL and
// every photo; left out it is derived.
func TestSubmitSlugIsOptional(t *testing.T) {
	photos := map[string][]byte{"one.jpg": samplePhoto(t)}
	base := map[string]string{
		"codeword": testCodeword,
		"name":     "Ada L",
		"license":  "CC-BY-4.0",
		"title":    "A Very Nice Shelf",
	}
	withSlug := map[string]string{}
	for k, v := range base {
		withSlug[k] = v
	}
	withSlug["slug"] = "Ada's Shelf!"

	// A make, with the slug overriding the title-derived one — and slugged, so
	// an apostrophe cannot reach the filename.
	h, up, prs := testHandler()
	rec := httptest.NewRecorder()
	h.Make(rec, formRequest(t, withSlug, photos))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if prs.path != "content/makes/2026-08-01-adas-shelf.md" {
		t.Errorf("path = %q, want the given slug", prs.path)
	}
	if !strings.Contains(prs.content, "slug: 'adas-shelf'") {
		t.Errorf("front matter missing the slug:\n%s", prs.content)
	}
	for key := range up.uploaded {
		if !strings.HasPrefix(key, "adas-shelf-001-") {
			t.Errorf("photo key = %q, want it to start with the given slug", key)
		}
	}

	// Photos, likewise: no slug means a derived one, a slug means that one.
	h, up, prs = testHandler()
	rec = httptest.NewRecorder()
	h.Photos(rec, formRequest(t, map[string]string{
		"codeword": testCodeword,
		"name":     "Ada L",
		"license":  "CC-BY-4.0",
		"slug":     "tuesday-tidy-up",
	}, photos))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if prs.path != "content/photos/2026-08-01-tuesday-tidy-up.md" {
		t.Errorf("path = %q, want the given slug", prs.path)
	}
	for key := range up.uploaded {
		if !strings.HasPrefix(key, "tuesday-tidy-up-001-") {
			t.Errorf("photo key = %q, want it to start with the given slug", key)
		}
	}
}

// A title is what distinguishes the two forms; the photos form must not need
// one, and the makes form must still insist on it.
func TestTitleIsOnlyRequiredForMakes(t *testing.T) {
	fields := map[string]string{
		"codeword": testCodeword,
		"name":     "Ada L",
		"license":  "CC-BY-4.0",
	}
	photos := map[string][]byte{"one.jpg": samplePhoto(t)}

	h, _, _ := testHandler()
	rec := httptest.NewRecorder()
	h.Photos(rec, formRequest(t, fields, photos))
	if rec.Code != http.StatusOK {
		t.Errorf("photos without a title: status = %d, want 200", rec.Code)
	}

	h, _, prs := testHandler()
	rec = httptest.NewRecorder()
	h.Make(rec, formRequest(t, fields, photos))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("make without a title: status = %d, want 400", rec.Code)
	}
	if prs.calls != 0 {
		t.Error("a titleless make opened a pull request")
	}
}

func TestSlugify(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"A Very Nice Shelf", "a-very-nice-shelf"},
		{"  spaces  everywhere  ", "spaces-everywhere"},
		{"Punctuation! Goes: away?", "punctuation-goes-away"},
		// An apostrophe joins rather than separates.
		{"Ada's Shelf", "adas-shelf"},
		{"Ada’s Shelf", "adas-shelf"},
		{"3D Printed Components", "3d-printed-components"},
		{"!!!", ""},
	} {
		if got := slugify(tc.in); got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestYAMLStringEscapes(t *testing.T) {
	// A title containing a quote must not break out of the front matter.
	got := buildMarkdown("Ada's \"Best\" Shelf", "adas-best-shelf", "", "Ada L", "CC-BY-4.0", []string{"a.jpg"}, time.Unix(0, 0).UTC())
	if !strings.Contains(got, "title: 'Ada''s \"Best\" Shelf'") {
		t.Errorf("quote not escaped:\n%s", got)
	}
}

// Every licence the form offers has to be one the server accepts, or a member
// picks it from the list and is told it is invalid. data/licenses.toml is not
// in the server image, so nothing but this test keeps the two in step.
func TestOfferedLicencesAreAccepted(t *testing.T) {
	raw, err := os.ReadFile("data/licenses.toml")
	if err != nil {
		t.Fatalf("reading the licence list: %v", err)
	}

	ids := regexp.MustCompile(`(?m)^\s*id\s*=\s*'([^']+)'`).FindAllStringSubmatch(string(raw), -1)
	if len(ids) == 0 {
		t.Fatal("found no licence ids in data/licenses.toml")
	}
	if len(ids) != len(licences) {
		t.Errorf("the form offers %d licences but the server accepts %d", len(ids), len(licences))
	}
	for _, match := range ids {
		if !licences[match[1]] {
			t.Errorf("the form offers %q, which the server rejects", match[1])
		}
	}
}
