# Contributing

## Build & test

This repository pins its local toolchain with [mise](https://mise.jdx.dev/).
Bootstrap the tools once, then run the normal gates through the mise environment:

```sh
mise run setup
mise exec -- make ci
```

`mise run setup` installs Go, Node, GNU Make, Zig (as the cgo compiler for
`go test -race`), golangci-lint, Helm, kubeconform, plus the Go helper binaries
used by the gates (`templ` and `govulncheck`) into a repo-local ignored
directory. `make ci` runs the local Go fast path (templ freshness, lint, comment
hygiene, and the race test suite); GitHub CI additionally runs vet,
`govulncheck`, frontend, Playwright, and chart jobs. Run the relevant local gates
before sending a patch. Use `mise run doctor` to print the active tool versions
when debugging local setup.

The frontend has a separate fast gate:

```sh
mise exec -- make frontend-check
```

It lints and typechecks both production and test TypeScript, runs Vitest with
the enforced V8 coverage floor, rebuilds the embedded assets, and fails if the
committed bundle is stale. Use `npm run test:watch` for the local Vitest loop.

The Playwright e2e target is intentionally heavier: `mise` provides Go/Node, but
`make e2e` may still need privileged OS browser dependencies via `npx playwright
install --with-deps chromium`. On machines where that is not desirable, use the
containerized `make e2e-docker` path after configuring Docker with enough memory
(see the Makefile preflight).

## Commits

Use `type: subject` (e.g. `fix: broken redirect`, `feat: add dark mode`).
Types: `feat`, `fix`, `chore`, `docs`, `style`, `test`, `ci`.

## License

readout is licensed under the **GNU GPL-3.0**. Contributions are accepted under
GPL-3.0; by submitting a patch you agree to license it under those terms.

## Out of scope

readout is **read-only by construction**: no write verbs, no mutating routes,
nothing that changes a cluster. Patches that add write-verb or mutating-route
behavior are out of scope and will not be accepted.
