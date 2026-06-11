# Makefile for the readout module.
#
# Quality gates (REQUIRED in CI): `make ci` runs templ-freshness, lint, and the
# race test suite -- the same three gates the GitHub workflow runs.

# Pinned templ codegen binary; must match the github.com/a-h/templ version in
# go.mod. `make tools` (re)installs it at the pinned version.
TEMPL_VERSION := v0.3.1020

.DEFAULT_GOAL := ci

.PHONY: ci tools generate templ-check lint test race build vet fmt air help e2e e2e-deps

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
