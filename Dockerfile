# syntax=docker/dockerfile:1.28.0@sha256:bb22d9815c728170f72750f4e5b0d672e06176142e1d602c7e66c050100b7e5b
# check=experimental=all;error=true

# Cross-compiling on the build platform rather than emulating the target: Go
# needs no toolchain in the target architecture, so emulation would only be
# slower.

# build compiles the static binary.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.2-alpine3.24@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build

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
FROM gcr.io/distroless/static-debian13:nonroot@sha256:516757106de35205efb70b91ba03f6f1298424a0678c603255cf2852a7000087 AS runtime

# --link keeps this layer independent of the stages above it, so bumping the
# base image does not invalidate it.
COPY --link --from=build /out/titelheld /titelheld

EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/titelheld"]
