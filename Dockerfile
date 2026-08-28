# golang:1.26.7-alpine, digest-pinned (tag in the ref is informational for Dependabot).
FROM golang:1.27.0-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS build
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
FROM gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
COPY --from=build /out/readout /readout
# 65532:65532 is the numeric UID:GID of distroless' `nonroot` user. It MUST be
# numeric, not the name `nonroot`: under Kubernetes runAsNonRoot the kubelet
# verifies non-root from the image's USER and cannot resolve a username to a UID,
# so a named USER fails admission with "non-numeric user" and the pod never
# starts. Numeric here lets the chart's runAsNonRoot pass with no runAsUser.
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/readout"]
