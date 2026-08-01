package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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
	return "https://github.com/chromy/makespace.org/pull/7", nil
}

func testHandler(auth Authenticator) (*submitHandler, *fakeUploader, *fakePRs) {
	up, prs := newFakeUploader(), &fakePRs{}
	h := &submitHandler{
		auth:     auth,
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
	h, up, prs := testHandler(devMember{name: "Riley P"})
	req := formRequest(t,
		map[string]string{"title": "A Very Nice Shelf", "body": "Made from offcuts."},
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
	if got.URL != "https://github.com/chromy/makespace.org/pull/7" {
		t.Errorf("URL = %q, want the pull request", got.URL)
	}

	if len(up.uploaded) != 1 {
		t.Fatalf("uploaded %d photos, want 1", len(up.uploaded))
	}
	var key string
	for k := range up.uploaded {
		key = k
	}
	if !strings.HasSuffix(key, ".jpg") || len(key) != 64+len(".jpg") {
		t.Errorf("key = %q, want a sha256 hex name with a .jpg suffix", key)
	}
	if up.types[key] != "image/jpeg" {
		t.Errorf("content type = %q, want image/jpeg", up.types[key])
	}

	if prs.path != "content/makes/a-very-nice-shelf.md" {
		t.Errorf("path = %q, want the slugged content path", prs.path)
	}
	for _, want := range []string{
		"title: 'A Very Nice Shelf'",
		"date: '2026-08-01T12:00:00Z'",
		"draft: false",
		"- 'Riley P'",
		"        - '" + key + "'",
		"Made from offcuts.",
	} {
		if !strings.Contains(prs.content, want) {
			t.Errorf("markdown missing %q:\n%s", want, prs.content)
		}
	}
}

// The bucket is public, so an unauthenticated caller must not be able to put
// objects in it or open pull requests.
func TestSubmitRequiresAMember(t *testing.T) {
	h, up, prs := testHandler(deniedAuth{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, formRequest(t,
		map[string]string{"title": "Sneaky"},
		map[string][]byte{"x.jpg": samplePhoto(t)}))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if len(up.uploaded) != 0 {
		t.Error("an unauthenticated submission uploaded a photo")
	}
	if prs.calls != 0 {
		t.Error("an unauthenticated submission opened a pull request")
	}
}

func TestSubmitValidation(t *testing.T) {
	photo := samplePhoto(t)
	for _, tc := range []struct {
		name   string
		fields map[string]string
		photos map[string][]byte
	}{
		{"no title", map[string]string{"body": "x"}, map[string][]byte{"a.jpg": photo}},
		{"no photos", map[string]string{"title": "Fine"}, nil},
		{"title with no letters", map[string]string{"title": "!!!"}, map[string][]byte{"a.jpg": photo}},
		{"title too long", map[string]string{"title": strings.Repeat("a", maxTitleRunes+1)}, map[string][]byte{"a.jpg": photo}},
		{"body too long", map[string]string{"title": "Fine", "body": strings.Repeat("b", maxBodyRunes+1)}, map[string][]byte{"a.jpg": photo}},
		{"not an image", map[string]string{"title": "Fine"}, map[string][]byte{"a.jpg": []byte("nope")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, prs := testHandler(devMember{name: "Riley P"})
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
// a pull request referencing a photo that is not there.
func TestSubmitStopsWhenUploadFails(t *testing.T) {
	h, up, prs := testHandler(devMember{name: "Riley P"})
	up.err = context.DeadlineExceeded

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, formRequest(t,
		map[string]string{"title": "Shelf"},
		map[string][]byte{"a.jpg": samplePhoto(t)}))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if prs.calls != 0 {
		t.Error("opened a pull request despite the upload failing")
	}
}

func TestSlugify(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"A Very Nice Shelf", "a-very-nice-shelf"},
		{"  spaces  everywhere  ", "spaces-everywhere"},
		{"Punctuation! Goes: away?", "punctuation-goes-away"},
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
	got := buildMarkdown("Riley's \"Best\" Shelf", "", "Riley P", []string{"a.jpg"}, time.Unix(0, 0).UTC())
	if !strings.Contains(got, "title: 'Riley''s \"Best\" Shelf'") {
		t.Errorf("quote not escaped:\n%s", got)
	}
}
