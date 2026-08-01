package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Path style rather than virtual-host style, because bucket.localhost does not
// resolve; Tigris itself is happy with the SDK's default.
func fakeBucket(t *testing.T, record func(*http.Request, []byte)) *bucketUploader {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		record(r, body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := s3.New(s3.Options{
		Region:       "auto",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("key", "secret", ""),
	})
	return &bucketUploader{client: client, bucket: "makespace-site-content"}
}

func TestUploadPutsObject(t *testing.T) {
	var gotPath, gotType, gotCache string
	var gotBody []byte
	up := fakeBucket(t, func(r *http.Request, body []byte) {
		gotPath, gotType, gotCache, gotBody = r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Cache-Control"), body
	})

	if err := up.Upload(context.Background(), "abc123.jpg", "image/jpeg", []byte("pretend jpeg")); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if want := "/makespace-site-content/abc123.jpg"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotType != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", gotType)
	}
	// Content-addressed keys never change bytes, and the site serves them with a
	// year of immutable caching; the object should say the same thing.
	if want := "public, max-age=31536000, immutable"; gotCache != want {
		t.Errorf("Cache-Control = %q, want %q", gotCache, want)
	}
	if string(gotBody) != "pretend jpeg" {
		t.Errorf("body = %q", gotBody)
	}
}

func TestUploadSurfacesFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	up := &bucketUploader{
		client: s3.New(s3.Options{
			Region:           "auto",
			BaseEndpoint:     aws.String(srv.URL),
			UsePathStyle:     true,
			Credentials:      credentials.NewStaticCredentialsProvider("key", "secret", ""),
			RetryMaxAttempts: 1,
		}),
		bucket: "makespace-site-content",
	}
	if err := up.Upload(context.Background(), "abc123.jpg", "image/jpeg", []byte("x")); err == nil {
		t.Error("Upload succeeded against a 403, want an error")
	}
}
