FROM golang:1.26-alpine AS build
WORKDIR /src
ARG VERSION=dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/kbelokon/readout/internal/version.Version=${VERSION}" \
    -o /out/readout ./cmd/readout

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/readout /readout
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/readout"]
