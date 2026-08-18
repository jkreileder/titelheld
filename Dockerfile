# Build stage. Pinned by digest like every other third-party artifact here;
# Renovate keeps the pins current.
FROM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS build

WORKDIR /src

# Dependencies first, so a source-only change reuses the layer.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

# CGO off and a static link, because the runtime image has no libc.
# -trimpath and an empty build id keep the binary reproducible.
ENV CGO_ENABLED=0
RUN go build -trimpath -buildvcs=false -ldflags="-s -w -buildid=" -o /out/titelheld ./cmd/titelheld

# Runtime stage: no shell, no package manager, no libc, and not root.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a

COPY --from=build /out/titelheld /titelheld

# Cloud Run sets PORT; this is the documented default the service also uses.
EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/titelheld"]
