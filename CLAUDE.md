# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

The public website for Makespace, a 24/7 makerspace in Cambridge (UK). A Hugo static site with a
single bundled theme (`themes/makespace/`) written specifically for this site — the theme is not
shared with anything else, so edit it directly rather than overriding it from the project root.

## Commands

```sh
aws s3 sync s3://makespace-site-content assets \
  --endpoint-url https://fly.storage.tigris.dev   # fetch photos — required before any build
hugo server                                       # dev server, http://localhost:1313/
hugo --gc --minify                                # production-ish build into public/
```

The sync needs `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` for the Tigris bucket in the
environment (`AWS_REGION=auto`).

There are no tests, linters, or package.json. CI installs Hugo extended (see
`.github/workflows/hugo.yml` for the pinned version) and deploys `public/` to GitHub Pages on every
push to `main`.

## Photos live in object storage, not git

`assets/` is gitignored and populated from the Tigris (Fly.io) bucket `makespace-site-content`,
an S3-compatible store reached with the AWS CLI at `https://fly.storage.tigris.dev`. Content front
matter references photos by bare filename (`params.images: ["8748039701.jpg"]`) and layouts resolve
them with `resources.Get`.

CI authenticates with the `TIGRIS_ACCESS_KEY_ID` / `TIGRIS_SECRET_ACCESS_KEY` repository secrets;
the endpoint, region and bucket name are plain `env` entries in the workflow.

**A build with an empty `assets/` fails hard**, not gracefully: `home.html` and `makes/single.html`
call `.Resize` on the result of `resources.Get` without a `with` guard, so a missing file is a
`nil pointer evaluating resource.Resource.Resize` error that aborts the whole render. If you hit
that, run the sync — it is usually missing photos, not a broken template. Adding a new make means
uploading its photos to the bucket as well as writing the markdown.

`resources/` (Hugo's resized derivatives) is gitignored alongside `assets/` and `public/`, and Hugo
regenerates it on demand. It used to be committed; the history was rewritten to drop it along with
the one photo that had been committed to `assets/`, which took the repo from 9.26 MiB to ~51 KiB.

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
