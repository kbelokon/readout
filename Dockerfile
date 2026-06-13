# golang:1.26-alpine, digest-pinned (tag in the ref is informational for Dependabot).
FROM golang:1.26-alpine@sha256:7a3e50096189ad57c9f9f865e7e4aa8585ed1585248513dc5cda498e2f41812c AS build
WORKDIR /src
ARG VERSION=dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/kbelokon/readout/internal/version.Version=${VERSION}" \
    -o /out/readout ./cmd/readout

# distroless static-debian12, tag :nonroot (informational, for Dependabot). The
# digest is authoritative. USER nonroot is set explicitly below regardless.
FROM gcr.io/distroless/static-debian12@sha256:9c346e4be81b5ca7ff31a0d89eaeade58b0f95cfd3baed1f36083ddb47ca3160
COPY --from=build /out/readout /readout
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/readout"]
