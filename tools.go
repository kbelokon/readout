//go:build tools

// Package tools anchors build/codegen tooling and supporting libraries in the
// module graph so `go mod tidy` keeps them as direct dependencies.
//
// This file is never compiled into the application binary or any test: the
// `tools` build tag excludes it from normal `go build`/`go test`. It exists
// only to pin:
//
//   - github.com/a-h/templ        — typed-template library; the matching
//     cmd/templ codegen binary is pinned to the SAME version via go install.
//   - github.com/a-h/templ/cmd/templ — the codegen binary, pinned here so the
//     library and binary versions cannot drift.
//   - github.com/alecthomas/chroma/v2 — YAML syntax highlighter.
//   - golang.org/x/sync/errgroup — bounded multi-cluster fan-out.
//   - sigs.k8s.io/yaml           — deterministic YAML serialization.
//   - github.com/PuerkitoBio/goquery — DOM-query assertions for the hermetic
//     behavior-fact tests.
package tools

import (
	_ "github.com/PuerkitoBio/goquery"
	_ "github.com/a-h/templ"
	_ "github.com/a-h/templ/cmd/templ"
	_ "github.com/alecthomas/chroma/v2"
	_ "golang.org/x/sync/errgroup"
	_ "sigs.k8s.io/yaml"
)
