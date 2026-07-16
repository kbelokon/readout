# golang:1.26-alpine, digest-pinned (tag in the ref is informational for Dependabot).
FROM golang:1.26-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS build
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
FROM gcr.io/distroless/static-debian12@sha256:61b7ccecebc7c474a531717de80a94709d20547cdcdaf740c25876f2a8e38b44
COPY --from=build /out/readout /readout
# 65532:65532 is the numeric UID:GID of distroless' `nonroot` user. It MUST be
# numeric, not the name `nonroot`: under Kubernetes runAsNonRoot the kubelet
# verifies non-root from the image's USER and cannot resolve a username to a UID,
# so a named USER fails admission with "non-numeric user" and the pod never
# starts. Numeric here lets the chart's runAsNonRoot pass with no runAsUser.
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/readout"]
