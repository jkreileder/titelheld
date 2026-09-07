# syntax=docker/dockerfile:1.27.0@sha256:bde3983e9c939224420ddaf6b784cc30e09b035a4dea01f581230c50809f372e
# check=experimental=all;error=true

# Cross-compiling on the build platform rather than emulating the target: Go
# needs no toolchain in the target architecture, so emulation would only be
# slower.

# build compiles the static binary.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.1-alpine3.24@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS build

ARG TARGETOS
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


# runtime holds the binary and nothing else.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7 AS runtime

# --link keeps this layer independent of the stages above it, so bumping the
# base image does not invalidate it.
COPY --link --from=build /out/titelheld /titelheld

EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/titelheld"]
