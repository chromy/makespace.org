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

	// What identifies the post, and therefore what its file is called: a title
	// for a make, the day it happened for a set of photos.
	var path string
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
		slug := slugify(title)
		if slug == "" {
			h.respond(w, r, http.StatusBadRequest, "That title needs at least one letter or number.", "")
			return
		}
		path = "content/makes/" + slug + ".md"

	case kindPhotos:
		// A date-only field, taken as midday so that no timezone shifts it onto
		// the day before or after.
		day, err := time.Parse(dateLayout, strings.TrimSpace(r.FormValue("date")))
		if err != nil {
			h.respond(w, r, http.StatusBadRequest, "Give the date the photos were taken.", "")
			return
		}
		date = time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, time.UTC)
		// Photos have no title of their own, so the page is named for its day.
		// A second set from the same day would collide, hence the suffix, which
		// is filled in once the first photo has been hashed.
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

	keys := make([]string, 0, len(files))
	for _, fh := range files {
		p, err := h.readPhoto(fh)
		if err != nil {
			// Something about the file itself: the member can fix this.
			log.Printf("submit: photo %q from %q: %v", fh.Filename, name, err)
			h.respond(w, r, http.StatusBadRequest,
				fmt.Sprintf("Could not accept %q: %s", fh.Filename, err), "")
			return
		}
		// Content-addressed: the same photo uploaded twice is one object, and a
		// key never points at different bytes later — which is what lets both the
		// bucket and the site serve it as immutable.
		sum := sha256.Sum256(p.data)
		key := hex.EncodeToString(sum[:]) + p.ext
		if err := h.uploader.Upload(r.Context(), key, p.contentType, p.data); err != nil {
			// Nothing wrong with the photo, so do not tell the member there is.
			log.Printf("submit: uploading %s for %q: %v", key, name, err)
			h.respond(w, r, http.StatusBadGateway,
				"The photos could not be stored just now. Try again in a minute.", "")
			return
		}
		keys = append(keys, key)
	}

	if kind == kindPhotos {
		// Now that the photos are hashed, the day can be made unique without
		// asking the member to name anything.
		path = fmt.Sprintf("content/photos/%s-%s.md", date.Format(dateLayout), keys[0][:8])
	}

	markdown := buildMarkdown(title, body, name, licence, keys, date)
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
func buildMarkdown(title, body, member, licence string, photos []string, now time.Time) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", yamlString(title))
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
