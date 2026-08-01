# syntax=docker/dockerfile:1

# Render the static site into /public.
#
# The Hugo version is pinned to match .github/workflows/hugo.yml — the image is
# the extended edition, which the theme's SCSS-free CSS pipeline does not
# strictly need, but keeping build and CI on one binary avoids surprises.
#
# --platform=$BUILDPLATFORM: rendered HTML is the same bytes whatever the target
# architecture, so this stage always runs natively rather than under emulation.
FROM --platform=$BUILDPLATFORM ghcr.io/gohugoio/hugo:v0.164.0 AS build

ARG HUGO_BASEURL=https://makespace.org/

# Front matter carries explicit UTC offsets, but the site has no `timeZone` set,
# so anything Hugo resolves against the local zone follows the container. Pin it
# to the zone CI has always built in rather than inheriting the image's UTC.
ENV TZ=Europe/London

# The image runs as hugo:hugo; the build needs to write resources/ and the
# image cache, so do the render as root.
USER root
WORKDIR /project

# Photos are not in the build context at all: Hugo fetches them from the public
# bucket while rendering, so this step needs network access.
COPY content/ ./content/
COPY static/ ./static/
COPY themes/ ./themes/
COPY hugo.toml ./

# HUGO_CACHEDIR is /cache in this image. The mount keeps both halves of the
# expensive work across builds: the fetched originals under filecache/getresource
# and the resized derivatives under images/.
RUN --mount=type=cache,target=/cache \
    hugo \
      --gc \
      --minify \
      --baseURL "${HUGO_BASEURL}" \
      --destination /public

# The rendered site on its own, for exporting back to the host:
#
#   docker build --target artifact --output type=local,dest=public .
#
# Exporting beats `docker create` + `docker cp` here because it never loads the
# ~500 MB Hugo toolchain image into the daemon just to read a directory out of it.
FROM scratch AS artifact
COPY --from=build /public /

# Compile the server. Dependencies are downloaded in their own layer so editing
# a .go file does not re-fetch the AWS SDK, and both caches are mounts: modules
# under /go/pkg/mod, compiled packages under the build cache.
# Also pinned to the build platform, then cross-compiled with GOARCH. Running
# the Go toolchain itself under QEMU crashes it outright — "hash of unhashable
# type" from the compiler — and cross-compiling is faster anyway.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS server-build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY *.go ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o /out/server .

# The image that actually runs: one static binary and the rendered site.
#
# distroless/static rather than scratch because the PR flow will call the GitHub
# API over TLS, and that needs the CA bundle this image carries. :nonroot runs
# as uid 65532, which is enough for a process that only ever reads files.
FROM gcr.io/distroless/static-debian12:nonroot AS serve
COPY --from=server-build /out/server /server
COPY --from=build /public /site
ENV SITE_DIR=/site
EXPOSE 8080
ENTRYPOINT ["/server"]
