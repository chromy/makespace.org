package main

import (
	"context"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
)

// uploadFiles puts photos into the bucket by hand, for the times the form is
// the wrong tool: seeding the bucket, or adding photos to a page being written
// in an editor. It prints a front matter block ready to paste.
//
// It deliberately goes through the same normalisePhoto as the form rather than
// putting the file up as-is. Uploading raw would publish whatever EXIF the
// camera wrote — including where the photo was taken, in a public bucket — and
// would produce a key that does not match the one the form would give the same
// image, so the same photo could end up stored twice.
func uploadFiles(ctx context.Context, up Uploader, baseURL, slug string, paths []string, out io.Writer) error {
	if len(paths) == 0 {
		return fmt.Errorf("name at least one image file to upload")
	}
	// The keys the forms write start with the post's slug, and these have to
	// match: a photo added by hand to a page should be named like one added
	// through the form.
	if slug = slugify(slug); slug == "" {
		return fmt.Errorf("give the post's slug with -slug, e.g. -slug a-very-nice-shelf")
	}

	keys := make([]string, 0, len(paths))
	for i, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(raw) > maxPhotoBytes {
			return fmt.Errorf("%s is larger than %d MB", path, maxPhotoBytes>>20)
		}
		p, err := normalisePhoto(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}

		// Numbered in the order given on the command line, as the form numbers
		// them in the order they were attached.
		key := photoKey(slug, i+1, photoHash(p), p.ext)
		if err := up.Upload(ctx, key, p.contentType, p.data); err != nil {
			return err
		}
		fmt.Fprintf(out, "%-16s -> %s\n", filepath.Base(path), strings.TrimSuffix(baseURL, "/")+"/"+key)
		keys = append(keys, key)
	}

	fmt.Fprintf(out, "\nparams:\n    images:\n")
	for _, key := range keys {
		fmt.Fprintf(out, "        - %s\n", yamlString(key))
	}
	return nil
}

// uploadAsset puts one file in the bucket byte for byte, under a key chosen by
// hand. This is for the site's own furniture — the licence badges — rather than
// for members' photos, which is why it skips normalisePhoto entirely: the
// badges are SVGs, and re-encoding one as a JPEG would be nonsense.
//
// Nothing here is content-addressed, so a key can be overwritten. That is the
// point for a badge, which should keep the same URL when Creative Commons
// redraws it, but it does mean these keys are not safe to cache forever the way
// a photo's is.
func uploadAsset(ctx context.Context, up Uploader, baseURL, key, path string, out io.Writer) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("give the key to store it under with -key")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("%s is empty", path)
	}

	contentType := mime.TypeByExtension(filepath.Ext(key))
	if contentType == "" {
		return fmt.Errorf("cannot tell the content type from the key %q — give it a file extension", key)
	}
	if err := up.Upload(ctx, key, contentType, data); err != nil {
		return err
	}
	fmt.Fprintf(out, "%-16s -> %s\n", filepath.Base(path), strings.TrimSuffix(baseURL, "/")+"/"+key)
	return nil
}
