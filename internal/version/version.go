package version

// Version is the build version. Release builds inject it via
// -ldflags "-X github.com/kbelokon/readout/internal/version.Version=<tag>".
// Plain `go build` and local runs keep the honest default below.
var Version = "dev"
