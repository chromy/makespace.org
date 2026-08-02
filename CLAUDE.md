# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

The public website for Makespace, a 24/7 makerspace in Cambridge (UK). A Hugo static site with a
single bundled theme (`themes/makespace/`) written specifically for this site — the theme is not
shared with anything else, so edit it directly rather than overriding it from the project root.

Alongside it, `main.go` is a small Go server that serves what Hugo renders. GitHub Pages could do
that job; the server exists because "create a PR to add new content" is moving off a Google Form and
onto the page, which needs somewhere to run a handler.

## Commands

```sh
hugo server                                       # dev server, http://localhost:1313/
hugo --gc --minify                                # production-ish build into public/

docker build --target artifact \
  --output type=local,dest=public .               # same build in Docker, exported to public/

go test ./...                                     # the server's tests
go run . -dir public                              # serve a rendered site, http://localhost:8080/
SUBMIT_CODEWORD=... GITHUB_CLIENT_ID=... \
  GITHUB_PRIVATE_KEY="$(cat key.pem)" \
  go run . -dir public                            # ...with submissions enabled
docker build -t makespace . && \
  docker run -p 8080:8080 makespace               # render and serve, as deployed
```

Every build needs network access, because Hugo fetches the photos as it renders. No credentials
are involved any more — the bucket is public.

There is no linter and no package.json. `go test ./...` covers the server; nothing tests the site
itself. On every push to `main`, CI runs the Go tests, builds the `serve` stage through the
`Dockerfile` — that is where the Hugo version is pinned, not in the workflow — pushes it to
`registry.fly.io/makespace-site` tagged with the commit SHA, and releases it with
`flyctl deploy --image`. Passing `--image` skips flyctl's own build, so what CI tested is exactly
what runs.

GitHub Pages is no longer a deploy target. The `artifact` stage stays because exporting `public/`
locally is still useful, but nothing in CI uses it.

Two caches sit behind that build, and they are not the same thing. `cache-from`/`cache-to:
type=gha` preserve BuildKit *layers*, which skips the render entirely when nothing changed. Hugo's
resized images live in a `--mount=type=cache`, whose contents no cache exporter carries, so
`buildkit-cache-dance` shuttles them between the mount and an `actions/cache` entry — without it,
every content edit re-encodes every photo.

## Deployment

The app is `makespace-site` in the `makespace-cambridge-ltd` org, region `lhr`, two shared-cpu-1x
machines with 512 MB each. It is live at **https://makespace-site.fly.dev/**. `fly.toml` holds the
service definition; the health check hits `/healthz`, and idle machines suspend rather than stop, so
a cold request resumes in milliseconds instead of booting.

Deploys need `FLY_API_TOKEN` — a Fly deploy token, already set as a repository secret — and nothing
else. `flyctl auth docker` turns the same token into registry credentials.

Two things about building the image are load-bearing, both learned by breaking them:

- **Fly machines are x86.** An arm64 image pushed from an Apple Silicon laptop is refused at release
  time with "does not support amd64 architecture", so the workflow pins `platforms: linux/amd64`.
- **Neither build stage runs under emulation.** Both are `FROM --platform=$BUILDPLATFORM`, and the
  server is cross-compiled with `GOOS`/`GOARCH` from `TARGETOS`/`TARGETARCH`. Rendered HTML is
  architecture-neutral, so Hugo has no reason to be emulated; the Go toolchain *cannot* be, as it
  panics under QEMU with "hash of unhashable type" before it compiles anything.

**DNS still points makespace.org at GitHub Pages.** Until that changes and `fly certs add` issues a
certificate, the Fly deployment is only reachable on its `.fly.dev` name, and the canonical site is
still the old static one. Because of that the site is **built with `baseURL =
https://makespace-site.fly.dev/`** — building with the real domain would emit absolute links, feeds
and a sitemap pointing at a different site. That value is set twice, in `hugo.toml` and as the
`HUGO_BASEURL` default in the `Dockerfile` (CI passes no build-arg, so the default is what ships).
Both change together when the domain moves.

## The server

`main.go` is deliberately small: a `http.FileServerFS` over the rendered site, plus middleware. New
routes go on the mux in `newHandler`, which is also what the tests drive.

- Cache headers key off the filename. Hugo content-addresses fingerprinted CSS
  (`main.min.<hash>.css`) and resized images (`<name>_hu_<hash>.jpg`), and those get a year of
  `immutable`; pages and feeds get `no-cache` and revalidate to a 304; everything else gets an hour.
- It serves the site's own `404.html` when Hugo renders one. It does not today — there is no 404
  template in the theme — so the wrapper is dormant and net/http's plain-text default shows instead.
- `-dir` (or `SITE_DIR`) points at the rendered site, `-addr` (or `PORT`) at the listen address. The
  site is read from disk, not embedded, so `go build` works in a fresh clone with no `public/`.
- The final image is `distroless/static:nonroot`, `static` rather than `scratch` for the CA bundle
  the GitHub API calls need.

## Submissions: form → bucket → pull request

`POST /submit` (`submit.go`) takes a multipart form — title, description, photos — and turns it into
a pull request against `content/makes/`. The page is `content/submit.md` with the
`themes/makespace/layouts/submit.html` layout, and it is a plain HTML form: the server answers with
a rendered page either way, so it works with JavaScript off.

The flow, and why each part is the way it is:

- **Photos are re-encoded, never stored as sent** (`image.go`). That strips EXIF, which on a phone
  photo carries the GPS position of whoever took it — and this bucket is public. The catch is
  orientation: phones store the sensor image unrotated and record the rotation in EXIF, so stripping
  the tag naively lays every portrait photo on its side. `applyOrientation` bakes the rotation into
  the pixels first. Re-encoding also proves the bytes really are an image before they get a URL.
- **Keys are `sha256(bytes).jpg`.** The same photo twice is one object, and a key never points at
  different bytes later — which is what justifies `immutable` caching on both the object and the
  site's own responses.
- **Photos are uploaded before the pull request exists.** A submission that is never merged still
  leaves objects in a public bucket. They are unreferenced and anonymous listing is denied, but they
  are not secret. This is inherent to serving photos from a public bucket at build time.
- **The PR is four REST calls** (`github.go`): read the base ref, create a branch, PUT the markdown
  through the contents API, open the PR. Photos are not committed; the markdown only names them.
  The branch carries a timestamp so two submissions of the same title do not collide, while the
  committed file keeps the clean slug — that name becomes the page's URL.
- **Identity is a GitHub App, not a PAT.** The server holds a private key and mints installation
  tokens that last an hour, scoped to this repo and revocable by uninstalling the app. The JWT is
  assembled by hand in `appJWT` — two base64 segments and an RS256 signature, no library.
- **The installation ID is discovered, not configured.** `GET /repos/{owner}/{repo}/installation`
  with the app JWT returns it, and it is cached for the life of the process. Three identifiers are
  easy to confuse here: the *client ID* names the app and is what `iss` carries; the *app ID* is an
  older numeric spelling of the same thing, which this server does not use; the *installation ID*
  names where the app is installed, and is the one the token endpoint needs. Only the first has to
  be configured.

**The licence is on the post, not the make.** The form offers a Creative Commons choice that covers
the words and photos on the page — a physical object is not what a copyright licence applies to, and
the form says so, because that is exactly what people get wrong. It lands in front matter as
`params.license: 'cc-by-sa-4.0'` and renders through `_partials/license.html`.

That list lives twice and has to stay in step: `data/licenses.toml` drives the form's options and
the rendered link; `licences` in `submit.go` is the allowlist the server validates against, because
the data file is not in the server image and an unvalidated id would be written straight into front
matter. `TestOfferedLicencesAreAccepted` reads the TOML and fails if the two drift. Adding a licence
means editing both. Pages written before the form existed carry no licence and render nothing, as
does an id the data file does not know.

**The gate is a shared codeword, not authentication.** The form carries a `codeword` field, compared
against `SUBMIT_CODEWORD` with `subtle.ConstantTimeCompare` before anything else is read — so a
wrong one costs no uploads and no API calls, and answers 403. It keeps drive-by bots out and does
nothing else: anyone holding it can submit under any name, so the `name` field that fills in
`members` is a claim rather than a verified identity. Review of the pull request is what actually
gates publication.

The codeword lives in the environment because this repository is public; committing it would
publish the thing it guards. Rotating it is `fly secrets set SUBMIT_CODEWORD=…`, which rolls the
machines automatically.

Runtime configuration, all Fly secrets: `GITHUB_CLIENT_ID`, `GITHUB_PRIVATE_KEY` (PEM, newlines
intact), `SUBMIT_CODEWORD`, plus `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` for *write* access to
the bucket. Optional overrides: `GITHUB_OWNER`, `GITHUB_REPO`, `GITHUB_BASE_BRANCH`, `BUCKET_NAME`,
`AWS_ENDPOINT_URL_S3`. With any of the three required values unset the server still serves the site
and `/submit` answers 503.

Failures are split by whose problem they are: a file that is too large, not an image, or too many
megapixels is 400 and names the file, because the member can fix it; a bucket that will not accept
the upload is 502 and says to try again, because blaming their photo for a storage outage would send
them off fixing the wrong thing.

## Photos live in object storage, not git

Photos live in the Tigris (Fly.io) bucket `makespace-site-content`, which is **public**: objects are
readable by anyone at `https://makespace-site-content.fly.storage.tigris.dev/<name>`, though
anonymous bucket *listing* is still denied. Nothing is synced to disk. Content front matter
references photos by bare filename (`params.images: ["8748039701.jpg"]`) and
`_partials/photo.html` turns that into a `resources.GetRemote` against `params.photoBaseURL` from
`hugo.toml`. Resizing works on the fetched resource exactly as it did on a local one.

Only the resized derivatives are published; originals never land in `public/`, so output is smaller
than it used to be. Fetched originals are cached in Hugo's `getresource` filecache under
`:cacheDir`, which in the container is the same `/cache` mount the resized images use — so CI's
cache dance covers downloads and encodes together.

Three things go through `_partials/photo.html`: the home page quilt, the photo block and share
button on a make, and the `{{< image name="..." >}}` shortcode for dropping a photo part-way
through a page's markdown. Anything else that wants a bucket photo should use it too.

The partial treats the two failure modes differently, and the distinction is the point:

- **404 → warn and skip.** The photo has not been uploaded yet. The make renders without it: no
  quilt tile, no photo block, and the Web Share button falls back to title and URL. Quiet, so watch
  the build log — a page that has lost its photos otherwise looks like a styling bug.
- **Anything else (403, DNS, connection refused) → `errorf`, which fails the build.** If the bucket
  goes private or unreachable, that must not deploy as a photo-less site.

That second rule has teeth. Immediately after the bucket was switched to public some edges still
answered 403 while others answered 404, and builds failed until it propagated — a minute or two.
`fly storage status makespace-site-content` shows the flag; `curl -o /dev/null -w '%{http_code}'`
against any missing key tells you what the edge actually thinks.

`assets/` is no longer part of the build at all. It stays in `.gitignore` so stray photos cannot be
committed. `resources/` (Hugo's resized derivatives) is gitignored too and regenerates on demand; it
used to be committed, and the history was rewritten to drop it along with the one photo that had
been committed to `assets/`, taking the repo from 9.26 MiB to ~51 KiB.

## Content model

One content section today, plus the standalone `content/submit.md`:

- `content/makes/` — member projects. Uses `params.images` (photo filenames), `members` (list of
  member names) and `params.license`. Rendered by the section-specific `layouts/makes/single.html`;
  anything else falls through to `layouts/page.html`.

`content/studies/` (case studies, front matter `author`) and `content/equipment/` (tools, front
matter `category`, `location`, `status`) were **removed for now** — both the content and their nav
groups. Their archetypes are still in `themes/makespace/archetypes/`, ready for when they come back;
the pages themselves are in git history (`git checkout <commit> -- content/studies content/equipment`).

`members` is a taxonomy (declared in `hugo.toml`), so each member gets a listing page at
`/members/<name>/`. It is the only taxonomy configured, though `page.html` and `makes/single.html`
also render a `tags` terms block that currently never has anything to show.

The `makes` archetype is a large aspirational template (equipment used, techniques, difficulty…)
that no existing page follows — real makes carry only `title`, `date`, `draft`, `members`,
`params.images` and a sentence of body text. Match the existing pages, not the archetype.

## Theme structure worth knowing

- `layouts/_partials/menu.html` does not use Hugo's menu system at all. It hardcodes two groups
  (Makes / More), listing every page in the section plus external links
  to the members' app and Google Forms. Adding a nav entry means editing that partial. The "More"
  group's own heading links to a `more` section that does not exist in `content/`, so its `href`
  renders empty — `site.GetPage` returns nil there and Hugo tolerates it silently.
- The logo is **not** in this repository. `_partials/header.html` fetches it from
  `github.com/Makespace/Branding` (`params.logoURL`, a raw.githubusercontent.com URL on `master`)
  so there is one canonical copy. Unlike a photo, a logo that cannot be fetched fails the build.
  It is 2000×1413 — *not* square, unlike the `static/logo.webp` it replaced — so the `width` and
  `height` attributes are computed from the resized resource; hardcoding a square squashes it. Note
  Hugo's `getresource` cache never expires, so a change in the branding repo is only picked up once
  that cache is dropped; `static/logo.webp` is now unreferenced but still published at `/logo.webp`.
- `layouts/_markup/render-link.html` adds `rel="external"` to any absolute link in markdown.
- Elements needing JavaScript get `class="needs-js"`; `_partials/head.html` hides them via a
  `<noscript>` style block. The only current user is the Web Share button in `makes/single.html`.
- CSS is a single hand-written `themes/makespace/assets/css/main.css`, unfingerprinted in
  development and minified + SRI-fingerprinted in production via `_partials/head/css.html`.
- **The stylesheet is intentionally almost empty**, so that a member who wants to style the site
  properly has room to. It declares variables in `:root` — colours, font stack, `--space`,
  `--sidebar` — and then only lays out what would be unusable without it: the page grid, the quilt,
  the photo grid on a make, the form. There is no decoration at all: no shadows, rounded corners,
  transitions, hover effects or font-size scale, and browser defaults are left to handle headings,
  code and blockquotes. When adding to it, prefer a new variable to a hardcoded value, and prefer
  restyling an element to inventing a class — the templates use nine classes in total. Two traps:
  the two-column breakpoint is repeated as a literal in the `@media` query because CSS cannot use a
  variable there, so it has to be changed in both places; and the markup carries no `.share-button`
  class, so the rules that used to style it never applied to anything.

`TODO.md` tracks the outstanding work on the site.
