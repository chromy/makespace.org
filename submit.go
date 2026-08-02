package main

import (
	"crypto/sha256"
	"crypto/subtle"
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
	maxNameRunes  = 80
	maxTitleRunes = 120
	maxBodyRunes  = 5000
	// The date field on the photos form: a plain day, as <input type="date"> sends it.
	dateLayout = "2006-01-02"
	// Headroom over the photos themselves for the text fields and MIME overhead.
	maxRequestBytes = maxPhotos*maxPhotoBytes + (1 << 20)
)

// licences the form may send, which is exactly what data/licenses.toml offers.
// An allowlist rather than a format check: the value is written into front
// matter and rendered as a link, so it has to be one the site can name a URL
// for. Adding a licence means adding it in both places.
//
// These cover the post — the words and photos on the page — not the object the
// member made.
var licences = map[string]bool{
	"cc-by-sa-4.0":    true,
	"cc-by-4.0":       true,
	"cc-by-nc-sa-4.0": true,
	"cc-by-nc-4.0":    true,
	"cc-by-nd-4.0":    true,
	"cc-by-nc-nd-4.0": true,
	"cc0-1.0":         true,
}

// submitHandler takes a filled-in form and turns it into a pull request: the
// photos go straight into the public bucket under content-addressed names, and
// the markdown that references them goes to GitHub for review.
//
// Note the ordering. Photos are uploaded before the pull request exists, so a
// submission that is never merged still leaves objects in a public bucket. They
// are unreferenced and the bucket cannot be listed anonymously, but they are
// not secret.
//
// The codeword is a shared secret typed into the form, not authentication: it
// keeps drive-by bots out, and nothing more. Anyone who has it can submit as
// any name, so the name in the front matter is a claim, not a verified identity
// — which is why every submission still goes through review as a pull request.
type submitHandler struct {
	codeword string
	uploader Uploader
	prs      PullRequestOpener
	now      func() time.Time
}

// The site takes two kinds of submission, which differ only in what identifies
// the post. A make is a project and is named by its title; a photo post is a
// few pictures from a particular day and is named by its date.
type submitKind int

const (
	kindMake submitKind = iota
	kindPhotos
)

// Make handles POST /submit — a project, with a title.
func (h *submitHandler) Make(w http.ResponseWriter, r *http.Request) {
	h.handle(w, r, kindMake)
}

// Photos handles POST /submit/photos — pictures with a date and some notes.
func (h *submitHandler) Photos(w http.ResponseWriter, r *http.Request) {
	h.handle(w, r, kindPhotos)
}

func (h *submitHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Make(w, r)
}

func (h *submitHandler) handle(w http.ResponseWriter, r *http.Request, kind submitKind) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		h.respond(w, r, http.StatusBadRequest, "That upload was too large or malformed.", "")
		return
	}
	defer r.MultipartForm.RemoveAll()

	// Checked before anything is read from the form, so a wrong codeword costs
	// no uploads and no API calls.
	if subtle.ConstantTimeCompare([]byte(r.FormValue("codeword")), []byte(h.codeword)) != 1 {
		h.respond(w, r, http.StatusForbidden,
			"That codeword is not right. Ask in the space if you need it.", "")
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	title := strings.TrimSpace(r.FormValue("title"))
	body := strings.TrimSpace(r.FormValue("body"))
	licence := strings.TrimSpace(r.FormValue("license"))
	switch {
	case !licences[licence]:
		h.respond(w, r, http.StatusBadRequest, "Choose a licence for the post.", "")
		return
	case name == "":
		h.respond(w, r, http.StatusBadRequest, "Put your name on it.", "")
		return
	case len([]rune(name)) > maxNameRunes:
		h.respond(w, r, http.StatusBadRequest, "That name is too long.", "")
		return
	case len([]rune(body)) > maxBodyRunes:
		h.respond(w, r, http.StatusBadRequest, "Those notes are too long.", "")
		return
	}

	// The slug names the page's file, its URL, and every photo attached to it.
	// It is optional on both forms: given one, it wins; otherwise it is derived
	// from whatever identifies the post.
	chosen := strings.TrimSpace(r.FormValue("slug"))
	slug := slugify(chosen)
	if chosen != "" && slug == "" {
		h.respond(w, r, http.StatusBadRequest, "That slug needs at least one letter or number.", "")
		return
	}

	// What identifies the post when no slug is given: a title for a make, the
	// day it happened for a set of photos.
	section := "makes"
	date := h.now()
	switch kind {
	case kindMake:
		switch {
		case title == "":
			h.respond(w, r, http.StatusBadRequest, "Give the make a title.", "")
			return
		case len([]rune(title)) > maxTitleRunes:
			h.respond(w, r, http.StatusBadRequest, "That title is too long.", "")
			return
		}
		if slug == "" {
			slug = slugify(title)
		}
		if slug == "" {
			h.respond(w, r, http.StatusBadRequest, "That title needs at least one letter or number.", "")
			return
		}

	case kindPhotos:
		section = "photos"
		// The form does not ask when the photos were taken — one fewer field to
		// fill in, and the answer is nearly always "today". The front matter
		// still carries a date so a reviewer can correct it in the pull request
		// when it is not; the file keeps the day it was posted either way.
		title = date.Format("2 January 2006")
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

	// Read and normalise everything before uploading anything: the photos are
	// named after the post, and a photo post with no slug is in turn named after
	// its first photo's hash, so nothing can be stored until all of it is known.
	photos := make([]photo, 0, len(files))
	for _, fh := range files {
		p, err := h.readPhoto(fh)
		if err != nil {
			// Something about the file itself: the member can fix this.
			log.Printf("submit: photo %q from %q: %v", fh.Filename, name, err)
			h.respond(w, r, http.StatusBadRequest,
				fmt.Sprintf("Could not accept %q: %s", fh.Filename, err), "")
			return
		}
		photos = append(photos, p)
	}

	if slug == "" {
		// Only reachable for a photo post with no slug: name it for the photos
		// themselves, which keeps two sets posted on the same day apart.
		slug = "photos-" + photoHash(photos[0])[:8]
	}

	keys := make([]string, 0, len(photos))
	for i, p := range photos {
		key := photoKey(slug, i+1, photoHash(p), p.ext)
		if err := h.uploader.Upload(r.Context(), key, p.contentType, p.data); err != nil {
			// Nothing wrong with the photo, so do not tell the member there is.
			log.Printf("submit: uploading %s for %q: %v", key, name, err)
			h.respond(w, r, http.StatusBadGateway,
				"The photos could not be stored just now. Try again in a minute.", "")
			return
		}
		keys = append(keys, key)
	}

	// Files are named for the day they were submitted, so a directory listing
	// reads chronologically. The URL keeps the bare slug via front matter, so it
	// does not carry a date the reader has no use for.
	path := fmt.Sprintf("content/%s/%s-%s.md", section, h.now().Format(dateLayout), slug)

	markdown := buildMarkdown(title, slug, body, name, licence, keys, date)
	prTitle := "Add " + map[submitKind]string{kindMake: "make: ", kindPhotos: "photos: "}[kind] + title
	prBody := fmt.Sprintf("Submitted by %s through the form on the site.\n\nPhotos are already in the bucket, so this can be previewed by building the branch.", name)

	url, err := h.prs.OpenPullRequest(r.Context(), path, markdown, prTitle, prBody)
	if err != nil {
		log.Printf("submit: opening pull request for %q: %v", title, err)
		h.respond(w, r, http.StatusBadGateway,
			"The photos were uploaded but the pull request could not be opened. Try again, or tell someone if it keeps happening.", "")
		return
	}

	log.Printf("submit: %q by %s -> %s", title, name, url)
	h.respond(w, r, http.StatusOK, "Thanks — your make is waiting for review.", url)
}

// photoKey names a photo after the post it came from, its position in that post
// and its content: `a-very-nice-shelf-001-<sha256>.jpg`.
//
// The hash is what matters for caching — a key still never points at different
// bytes later, so both the bucket and the site can serve it as immutable. The
// slug and index are there for the human staring at a bucket listing, and they
// cost the one thing a pure content hash gave for free: the same photo attached
// to two posts is now stored twice under two names.
func photoKey(slug string, index int, hash, ext string) string {
	return fmt.Sprintf("%s-%03d-%s%s", slug, index, hash, ext)
}

func photoHash(p photo) string {
	sum := sha256.Sum256(p.data)
	return hex.EncodeToString(sum[:])
}

// readPhoto reads one uploaded file and normalises it. Every error it returns
// is the member's to fix, which is what lets the caller answer 400 for these
// and 502 for a failed upload.
func (h *submitHandler) readPhoto(fh *multipart.FileHeader) (photo, error) {
	if fh.Size > maxPhotoBytes {
		return photo{}, fmt.Errorf("it is larger than %d MB", maxPhotoBytes>>20)
	}
	f, err := fh.Open()
	if err != nil {
		return photo{}, err
	}
	defer f.Close()

	raw, err := io.ReadAll(io.LimitReader(f, maxPhotoBytes+1))
	if err != nil {
		return photo{}, err
	}
	if len(raw) > maxPhotoBytes {
		return photo{}, fmt.Errorf("it is larger than %d MB", maxPhotoBytes>>20)
	}
	return normalisePhoto(raw)
}

// buildMarkdown writes the front matter the makes section expects: title, date,
// draft, members, params.license and params.images, then the description as the
// body.
func buildMarkdown(title, slug, body, member, licence string, photos []string, now time.Time) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", yamlString(title))
	// The file is named <posted date>-<slug>, so without this the date would
	// end up in the URL too. It is the date the photos were taken, or the make
	// was submitted; the filename carries the posting date.
	fmt.Fprintf(&b, "slug: %s\n", yamlString(slug))
	fmt.Fprintf(&b, "date: %s\n", yamlString(now.Format(time.RFC3339)))
	b.WriteString("draft: false\n")
	b.WriteString("members:\n")
	fmt.Fprintf(&b, "    - %s\n", yamlString(member))
	b.WriteString("params:\n")
	fmt.Fprintf(&b, "    license: %s\n", yamlString(licence))
	b.WriteString("    images:\n")
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
		// An apostrophe joins a word rather than breaking it: Ada's Shelf is
		// adas-shelf, not ada-s-shelf.
		case r == '\'' || r == '’':
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
