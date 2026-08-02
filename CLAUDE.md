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
go run . -dir public -dev-member 'Riley P'        # ...with submissions accepted as that member
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
still the old static one.

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

Runtime configuration, all Fly secrets: `GITHUB_CLIENT_ID` and
`GITHUB_PRIVATE_KEY` (PEM, newlines intact), plus `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` for
*write* access to the bucket. Optional overrides: `GITHUB_OWNER`, `GITHUB_REPO`,
`GITHUB_BASE_BRANCH`, `BUCKET_NAME`, `AWS_ENDPOINT_URL_S3`. With the GitHub pair unset the server
still serves the site and `/submit` answers 503.

**The identity check is not implemented, and submissions are refused because of it.**
`Authenticator` (`auth.go`) is an interface whose only production implementation, `deniedAuth`,
turns everyone away. app.makespace.org exposes no OAuth or OIDC endpoint — just its own `/log-in` —
so there is nothing to verify a session against. Bridging it needs a change on the members' app
side, either an OAuth2 authorization-code flow or a signed hand-off token with a shared secret. For
local work, `-dev-member "Riley P"` accepts anyone as that member; never set it in production.

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

Three content sections, each with its own archetype in `themes/makespace/archetypes/`:

- `content/makes/` — member projects. Uses `params.images` (photo filenames) and `members` (list of
  member names). Rendered by the section-specific `layouts/makes/single.html`; other sections fall
  through to `layouts/page.html`.
- `content/studies/` — case studies about local companies. Front matter `author`.
- `content/equipment/` — tools in the space. Front matter `category`, `location`, `status`.

`members` is a taxonomy (declared in `hugo.toml`), so each member gets a listing page at
`/members/<name>/`. It is the only taxonomy configured, though `page.html` and `makes/single.html`
also render a `tags` terms block that currently never has anything to show.

The `makes` archetype is a large aspirational template (equipment used, techniques, difficulty…)
that no existing page follows — real makes carry only `title`, `date`, `draft`, `members`,
`params.images` and a sentence of body text. Match the existing pages, not the archetype.

## Theme structure worth knowing

- `layouts/_partials/menu.html` does not use Hugo's menu system at all. It hardcodes four groups
  (Makes / Case studies / Equipment / More), listing every page in each section plus external links
  to the members' app and Google Forms. Adding a nav entry means editing that partial. The "More"
  group's own heading links to a `more` section that does not exist in `content/`, so its `href`
  renders empty — `site.GetPage` returns nil there and Hugo tolerates it silently.
- `layouts/_markup/render-link.html` adds `rel="external"` to any absolute link in markdown.
- Elements needing JavaScript get `class="needs-js"`; `_partials/head.html` hides them via a
  `<noscript>` style block. The only current user is the Web Share button in `makes/single.html`.
- CSS is a single hand-written `themes/makespace/assets/css/main.css`, unfingerprinted in
  development and minified + SRI-fingerprinted in production via `_partials/head/css.html`.

`TODO.md` tracks the outstanding work on the site.
