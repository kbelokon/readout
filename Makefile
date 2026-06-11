# Makefile for the readout module.
#
# Quality gates (REQUIRED in CI): `make ci` runs templ-freshness, lint, and the
# race test suite -- the same three gates the GitHub workflow runs.

# Pinned templ codegen binary; must match the github.com/a-h/templ version in
# go.mod. `make tools` (re)installs it at the pinned version.
TEMPL_VERSION := v0.3.1020

# Official Playwright image, pinned to the @playwright/test version in
# tests/e2e/package.json. The `e2e-docker` target verifies the two agree before
# running, so the pin cannot drift silently. Always linux/amd64 (the local
# daemon is arm64; Rosetta emulation runs it).
PLAYWRIGHT_IMAGE := mcr.microsoft.com/playwright:v1.60.0-noble

.DEFAULT_GOAL := ci

.PHONY: ci tools generate templ-check lint test race build vet fmt air help e2e e2e-deps e2e-docker

## ci: the REQUIRED gates -- templ freshness, lint, race tests (matches .github/workflows/ci.yaml)
ci: templ-check lint race

## tools: install the pinned templ codegen binary (into $(go env GOBIN))
tools:
	go install github.com/a-h/templ/cmd/templ@$(TEMPL_VERSION)

## generate: run templ codegen over *.templ files
generate:
	templ generate

## templ-check: regenerate templ output and fail if it drifts from what is committed
templ-check: generate
	@git diff --exit-code -- '*_templ.go' \
		|| { echo 'ERROR: templ output is stale -- run `make generate` and commit the result.'; exit 1; }

## lint: golangci-lint (v2 config in .golangci.yml)
lint:
	golangci-lint run ./...

## test: plain test suite
test:
	go test ./...

## race: the REQUIRED race-detector test gate
race:
	go test ./... -race

## build: compile everything
build:
	go build ./...

## vet: go vet
vet:
	go vet ./...

## fmt: apply the configured formatters in place
fmt:
	golangci-lint fmt ./...

## e2e: build readout and run the Playwright suite against the fakeapi harness (deliberately NOT part of `make ci`)
e2e: e2e-deps
	go build -o readout ./cmd/readout
	cd tests/e2e && READOUT_BIN=$(CURDIR)/readout npx playwright test

## e2e-docker: run the e2e suite inside the pinned Playwright image (linux/amd64) with prebuilt Go binaries
e2e-docker:
	@image_tag=$$(printf '%s' '$(PLAYWRIGHT_IMAGE)' | sed -E 's/.*:v([0-9.]+)-.*/\1/'); \
		pkg_ver=$$(node -p "require('./tests/e2e/package.json').devDependencies['@playwright/test']"); \
		if [ "$$image_tag" != "$$pkg_ver" ]; then \
			echo "ERROR: PLAYWRIGHT_IMAGE pins v$$image_tag but tests/e2e/package.json wants @playwright/test $$pkg_ver -- bump them together."; \
			exit 1; \
		fi
	# The Playwright image has no Go toolchain, so cross-compile the readout and
	# harness binaries on the host (CGO_ENABLED=0 -> static linux/amd64) and mount
	# them in. HARNESS_BIN/READOUT_BIN point the harness at the prebuilt binaries.
	mkdir -p .build/linux-amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o .build/linux-amd64/readout ./cmd/readout
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o .build/linux-amd64/harness ./tests/e2e/harness
	docker run --rm --platform linux/amd64 -v $(CURDIR):/work -w /work/tests/e2e $(PLAYWRIGHT_IMAGE) \
		sh -c 'npm ci --no-audit --no-fund && HARNESS_BIN=/work/.build/linux-amd64/harness READOUT_BIN=/work/.build/linux-amd64/readout npx playwright test'

## e2e-deps: install the e2e suite's npm deps and Chromium (both steps are idempotent)
e2e-deps:
	cd tests/e2e && npm install --no-audit --no-fund
	cd tests/e2e && npx playwright install --with-deps chromium

## air: local live-reload dev server (dev-only; not a CI gate)
air:
	air

## help: list documented targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
