package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const testBucketURL = "https://makespace-site-content.fly.storage.tigris.dev/"

func TestUploadFilesNamesAndNumbersPhotos(t *testing.T) {
	up := newFakeUploader()
	first := writeTemp(t, "one.jpg", samplePhoto(t))
	second := writeTemp(t, "two.jpg", jpegWithOrientation(t, landscape(), 3))

	var out bytes.Buffer
	if err := uploadFiles(context.Background(), up, testBucketURL, "a-very-nice-shelf",
		[]string{first, second}, &out); err != nil {
		t.Fatalf("uploadFiles: %v", err)
	}

	if len(up.uploaded) != 2 {
		t.Fatalf("uploaded %d files, want 2", len(up.uploaded))
	}
	// slug-XXX-sha256.ext, numbered in the order given.
	pattern := regexp.MustCompile(`^a-very-nice-shelf-00[12]-[0-9a-f]{64}\.jpg$`)
	sequence := map[string]bool{}
	for key := range up.uploaded {
		if !pattern.MatchString(key) {
			t.Errorf("key %q does not match slug-XXX-sha256.ext", key)
		}
		sequence[key[len("a-very-nice-shelf-"):][:3]] = true
	}
	if !sequence["001"] || !sequence["002"] {
		t.Errorf("photos are numbered %v, want 001 and 002", sequence)
	}

	if !strings.Contains(out.String(), testBucketURL+"a-very-nice-shelf-001-") {
		t.Errorf("output does not show the public URL:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "params:\n    images:\n        - 'a-very-nice-shelf-001-") {
		t.Errorf("output is not pasteable front matter:\n%s", out.String())
	}
}

// The script and the form must agree on the key, or the same photo added both
// ways becomes two objects under two names.
func TestUploadFilesAgreesWithTheFormOnKeys(t *testing.T) {
	raw := samplePhoto(t)
	viaForm, err := normalisePhoto(raw)
	if err != nil {
		t.Fatal(err)
	}

	up := newFakeUploader()
	var out bytes.Buffer
	if err := uploadFiles(context.Background(), up, testBucketURL, "a-shelf",
		[]string{writeTemp(t, "same.jpg", raw)}, &out); err != nil {
		t.Fatal(err)
	}

	want := photoKey("a-shelf", 1, photoHash(viaForm), viaForm.ext)
	if _, ok := up.uploaded[want]; !ok {
		t.Errorf("script stored %v, want the key the form would use (%s)", keysOf(up), want)
	}
}

// Uploading a file untouched would publish its EXIF, so the script has to
// re-encode exactly as the form does.
func TestUploadFilesStripsEXIF(t *testing.T) {
	up := newFakeUploader()
	var out bytes.Buffer
	path := writeTemp(t, "phone.jpg", jpegWithOrientation(t, landscape(), 6))

	if err := uploadFiles(context.Background(), up, testBucketURL, "a-shelf", []string{path}, &out); err != nil {
		t.Fatal(err)
	}
	for key, data := range up.uploaded {
		if exifSegment(data) != nil {
			t.Errorf("%s still carries EXIF", key)
		}
	}
}

func TestUploadFilesRejectsRubbish(t *testing.T) {
	up := newFakeUploader()
	var out bytes.Buffer
	photo := writeTemp(t, "one.jpg", samplePhoto(t))

	for _, tc := range []struct {
		name  string
		slug  string
		paths []string
	}{
		{"no files", "a-shelf", nil},
		{"no slug", "", []string{photo}},
		{"slug with no letters or digits", "!!!", []string{photo}},
		{"not an image", "a-shelf", []string{writeTemp(t, "notes.txt", []byte("not an image"))}},
		{"missing file", "a-shelf", []string{"/nowhere/at/all.jpg"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := uploadFiles(context.Background(), up, testBucketURL, tc.slug, tc.paths, &out); err == nil {
				t.Error("uploadFiles succeeded, want an error")
			}
		})
	}
	if len(up.uploaded) != 0 {
		t.Errorf("uploaded %v despite every case being invalid", keysOf(up))
	}
}

// The licence badges are SVGs stored at keys the templates name, so this path
// must not touch the bytes or invent a key.
func TestUploadAssetStoresVerbatim(t *testing.T) {
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 42"></svg>`)
	up := newFakeUploader()
	var out bytes.Buffer

	if err := uploadAsset(context.Background(), up, testBucketURL,
		"licenses/by.svg", writeTemp(t, "by.svg", svg), &out); err != nil {
		t.Fatalf("uploadAsset: %v", err)
	}

	stored, ok := up.uploaded["licenses/by.svg"]
	if !ok {
		t.Fatalf("stored %v, want the key it was given", keysOf(up))
	}
	if !bytes.Equal(stored, svg) {
		t.Error("the bytes were changed on the way in")
	}
	if got := up.types["licenses/by.svg"]; got != "image/svg+xml" {
		t.Errorf("content type = %q, want image/svg+xml", got)
	}
	if !strings.Contains(out.String(), testBucketURL+"licenses/by.svg") {
		t.Errorf("output does not show the public URL:\n%s", out.String())
	}
}

func TestUploadAssetRejectsRubbish(t *testing.T) {
	up := newFakeUploader()
	var out bytes.Buffer
	file := writeTemp(t, "by.svg", []byte("<svg/>"))

	for _, tc := range []struct{ name, key, path string }{
		{"no key", "", file},
		{"key with no extension", "licenses/by", file},
		{"missing file", "licenses/by.svg", "/nowhere/by.svg"},
		{"empty file", "licenses/by.svg", writeTemp(t, "empty.svg", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := uploadAsset(context.Background(), up, testBucketURL, tc.key, tc.path, &out); err == nil {
				t.Error("uploadAsset succeeded, want an error")
			}
		})
	}
	if len(up.uploaded) != 0 {
		t.Errorf("stored %v despite every case being invalid", keysOf(up))
	}
}

func keysOf(up *fakeUploader) []string {
	var keys []string
	for k := range up.uploaded {
		keys = append(keys, k)
	}
	return keys
}
