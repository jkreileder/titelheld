# syntax=docker/dockerfile:1.26.0@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
# check=experimental=all;error=true

# Cross-compiling on the build platform rather than emulating the target: Go
# needs no toolchain in the target architecture, so emulation would only be
# slower.

# build compiles the static binary.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.6-alpine3.24@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS build

# TARGETOS is supplied by BuildKit and names the target operating system.
ARG TARGETOS
# TARGETARCH is supplied by BuildKit and names the target architecture.
ARG TARGETARCH

WORKDIR /src

# Nothing is copied into the build stage. The sources are bind-mounted for the
# duration of the command, so no layer holds them and the build context cannot
# end up in an image; the module and build caches are mounts too, so a rebuild
# reuses them without baking them in.
#
# CGO off and a static link, because the runtime image has no libc. -trimpath
# and an empty build id keep the binary reproducible.
RUN --mount=type=bind,target=/src \
    --mount=type=cache,id=go-mod,target=/go/pkg/mod \
    --mount=type=cache,id=go-build,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o /out/titelheld ./cmd/titelheld

# No shell, no package manager, no libc, no root user.

# runtime holds the binary and nothing else.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6 AS runtime

# --link keeps this layer independent of the stages above it, so bumping the
# base image does not invalidate it.
COPY --link --from=build /out/titelheld /titelheld

EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/titelheld"]
