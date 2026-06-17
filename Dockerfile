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
# digest is authoritative. The base image's own USER is 0 (root), so we set the
# non-root user explicitly below regardless.
FROM gcr.io/distroless/static-debian12@sha256:9c346e4be81b5ca7ff31a0d89eaeade58b0f95cfd3baed1f36083ddb47ca3160
COPY --from=build /out/readout /readout
# 65532:65532 is the numeric UID:GID of distroless' `nonroot` user. It MUST be
# numeric, not the name `nonroot`: under Kubernetes runAsNonRoot the kubelet
# verifies non-root from the image's USER and cannot resolve a username to a UID,
# so a named USER fails admission with "non-numeric user" and the pod never
# starts. Numeric here lets the chart's runAsNonRoot pass with no runAsUser.
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/readout"]
