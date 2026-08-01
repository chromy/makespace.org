package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
	"unicode"
)

const (
	maxTitleRunes = 120
	maxBodyRunes  = 5000
	// Headroom over the photos themselves for the text fields and MIME overhead.
	maxRequestBytes = maxPhotos*maxPhotoBytes + (1 << 20)
)

// submitHandler takes a filled-in form and turns it into a pull request: the
// photos go straight into the public bucket under content-addressed names, and
// the markdown that references them goes to GitHub for review.
//
// Note the ordering. Photos are uploaded before the pull request exists, so a
// submission that is never merged still leaves objects in a public bucket. They
// are unreferenced and the bucket cannot be listed anonymously, but they are
// not secret.
type submitHandler struct {
	auth     Authenticator
	uploader Uploader
	prs      PullRequestOpener
	now      func() time.Time
}

func (h *submitHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	member, ok := h.auth.Member(r)
	if !ok {
		h.respond(w, r, http.StatusUnauthorized,
			"Submissions are open to Makespace members. Sign in to the members' app first.", "")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		h.respond(w, r, http.StatusBadRequest, "That upload was too large or malformed.", "")
		return
	}
	defer r.MultipartForm.RemoveAll()

	title := strings.TrimSpace(r.FormValue("title"))
	body := strings.TrimSpace(r.FormValue("body"))
	switch {
	case title == "":
		h.respond(w, r, http.StatusBadRequest, "Give the make a title.", "")
		return
	case len([]rune(title)) > maxTitleRunes:
		h.respond(w, r, http.StatusBadRequest, "That title is too long.", "")
		return
	case len([]rune(body)) > maxBodyRunes:
		h.respond(w, r, http.StatusBadRequest, "That description is too long.", "")
		return
	}

	slug := slugify(title)
	if slug == "" {
		h.respond(w, r, http.StatusBadRequest, "That title needs at least one letter or number.", "")
		return
	}

	files := r.MultipartForm.File["photos"]
	if len(files) == 0 {
		h.respond(w, r, http.StatusBadRequest, "Add at least one photo.", "")
		return
	}
	if len(files) > maxPhotos {
		h.respond(w, r, http.StatusBadRequest,
			fmt.Sprintf("That is more than %d photos.", maxPhotos), "")
		return
	}

	keys := make([]string, 0, len(files))
	for _, fh := range files {
		key, err := h.storePhoto(r, fh)
		if err != nil {
			log.Printf("submit: photo %q from %q: %v", fh.Filename, member.Name, err)
			h.respond(w, r, http.StatusBadRequest,
				fmt.Sprintf("Could not accept %q: %s", fh.Filename, err), "")
			return
		}
		keys = append(keys, key)
	}

	path := "content/makes/" + slug + ".md"
	markdown := buildMarkdown(title, body, member.Name, keys, h.now())
	prTitle := "Add make: " + title
	prBody := fmt.Sprintf("Submitted by %s through the form on the site.\n\nPhotos are already in the bucket, so this can be previewed by building the branch.", member.Name)

	url, err := h.prs.OpenPullRequest(r.Context(), path, markdown, prTitle, prBody)
	if err != nil {
		log.Printf("submit: opening pull request for %q: %v", title, err)
		h.respond(w, r, http.StatusBadGateway,
			"The photos were uploaded but the pull request could not be opened. Try again, or tell someone if it keeps happening.", "")
		return
	}

	log.Printf("submit: %q by %s -> %s", title, member.Name, url)
	h.respond(w, r, http.StatusOK, "Thanks — your make is waiting for review.", url)
}

func (h *submitHandler) storePhoto(r *http.Request, fh *multipart.FileHeader) (string, error) {
	if fh.Size > maxPhotoBytes {
		return "", fmt.Errorf("it is larger than %d MB", maxPhotoBytes>>20)
	}
	f, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer f.Close()

	raw, err := io.ReadAll(io.LimitReader(f, maxPhotoBytes+1))
	if err != nil {
		return "", err
	}
	if len(raw) > maxPhotoBytes {
		return "", fmt.Errorf("it is larger than %d MB", maxPhotoBytes>>20)
	}

	p, err := normalisePhoto(raw)
	if err != nil {
		return "", err
	}
	// Content-addressed: the same photo uploaded twice is one object, and a key
	// never points at different bytes later — which is what lets both the bucket
	// and the site serve it as immutable.
	sum := sha256.Sum256(p.data)
	key := hex.EncodeToString(sum[:]) + p.ext
	if err := h.uploader.Upload(r.Context(), key, p.contentType, p.data); err != nil {
		return "", err
	}
	return key, nil
}

// buildMarkdown writes the front matter the makes section expects: title, date,
// draft, members and params.images, then the description as the body.
func buildMarkdown(title, body, member string, photos []string, now time.Time) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", yamlString(title))
	fmt.Fprintf(&b, "date: %s\n", yamlString(now.Format(time.RFC3339)))
	b.WriteString("draft: false\n")
	b.WriteString("members:\n")
	fmt.Fprintf(&b, "    - %s\n", yamlString(member))
	b.WriteString("params:\n    images:\n")
	for _, p := range photos {
		fmt.Fprintf(&b, "        - %s\n", yamlString(p))
	}
	b.WriteString("---\n")
	if body != "" {
		b.WriteString("\n" + body + "\n")
	}
	return b.String()
}

// yamlString single-quotes a scalar, which in YAML escapes everything except a
// single quote — doubled.
func yamlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func slugify(title string) string {
	var b strings.Builder
	lastDash := true // suppresses a leading dash
	for _, r := range strings.ToLower(title) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteRune('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

var resultPage = template.Must(template.New("result").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>{{ .Message }}</title></head>
<body><h1>{{ .Message }}</h1>
{{ with .URL }}<p><a href="{{ . }}">See the pull request</a></p>{{ end }}
<p><a href="/">Back to the site</a></p>
</body></html>
`))

func (h *submitHandler) respond(w http.ResponseWriter, r *http.Request, status int, message, url string) {
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		fmt.Fprintf(w, "{%q:%q,%q:%q}\n", "message", message, "url", url)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = resultPage.Execute(w, struct{ Message, URL string }{message, url})
}
