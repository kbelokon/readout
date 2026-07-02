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

.PHONY: ci tools generate templ-check lint comment-check test race build vet fmt air help e2e e2e-deps e2e-docker e2e-visual e2e-visual-update assets assets-check

## ci: the REQUIRED gates -- templ freshness, lint, comment hygiene, race tests (matches .github/workflows/ci.yaml)
ci: templ-check lint comment-check race

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

## comment-check: fail on design-doc references in code comments (see scripts/check-comments.sh)
comment-check:
	bash scripts/check-comments.sh

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

## assets: rebuild the embedded frontend artifacts from internal/assets/src and typecheck them (npm ci on first run)
# Frontend build gate, the mirror of templ codegen for the static/ artifacts:
# esbuild + Lightning CSS regenerate internal/assets/static/readout.{js,css} from
# the src tree, then BOTH typecheckers run (tsgo is the fast TS7 native preview,
# tsc is the stable cross-check -- kept until TS7 is stable). Deliberately NOT a
# `make ci` gate -- `make ci` stays Go-only; the frontend lives in CI's separate
# `frontend` job. node_modules is installed via `npm ci` only when absent.
assets:
	@test -d node_modules || npm ci
	node scripts/build-assets.mjs
	npx tsgo --noEmit
	npx tsc --noEmit

## assets-check: rebuild the artifacts and fail if they drift from what is committed (the freshness gate)
assets-check: assets
	@git diff --exit-code -- internal/assets/static \
		|| { echo 'ERROR: asset output is stale -- run `make assets` and commit the result.'; exit 1; }

## e2e: build readout and run the Playwright suite against the fakeapi harness (deliberately NOT part of `make ci`)
e2e: e2e-deps
	go build -o readout ./cmd/readout
	cd tests/e2e && READOUT_BIN=$(CURDIR)/readout npx playwright test

## e2e-docker: run the e2e suite inside the pinned Playwright image (linux/amd64) with prebuilt Go binaries
e2e-docker: e2e-docker-preflight e2e-docker-binaries
	docker run --rm --platform linux/amd64 -v $(CURDIR):/work -w /work/tests/e2e $(PLAYWRIGHT_IMAGE) \
		sh -c 'npm ci --no-audit --no-fund && HARNESS_BIN=/work/.build/linux-amd64/harness READOUT_BIN=/work/.build/linux-amd64/readout npx playwright test'

## e2e-visual: run the SPEC §9 visual baselines (the `visual` project) on the HOST -- compares against committed PNGs
# Host-only, single-machine contract: the baselines are the developer machine's
# own Chromium render, so a near-strict compare is honest on that one machine.
# Chromium glyph rasterization is NOT deterministic across machines (nor under
# Rosetta emulation), so these PNGs are not portable -- regenerate them with
# `make e2e-visual-update` whenever the dev mac or its macOS version changes. CI
# does NOT run the visual grid.
#
# RO_VISUAL_MAXDIFF=180 is the MEASURED same-machine glyph-edge noise floor: the
# clean frames peaked at 57 differing pixels across repeated strict runs, x3 for
# margin. The wall-of-text logs frames (which flipped 0<->~14k px) are masked in
# the spec instead, so no _DENSE budget is needed here -- the env mechanism still
# lives in playwright.config.ts (default 0) as an escape hatch.
e2e-visual: e2e-deps
	go build -o readout ./cmd/readout
	cd tests/e2e && READOUT_BIN=$(CURDIR)/readout RO_VISUAL=1 RO_VISUAL_MAXDIFF=180 npx playwright test --project=visual

## e2e-visual-update: REGENERATE the visual baselines on the HOST (commit the result)
# Generation mode (--update-snapshots): writes the PNGs and reports pass
# regardless of any diff. Run this on the dev mac after changing machines or
# updating macOS, then commit the refreshed tests/e2e/__screenshots__.
e2e-visual-update: e2e-deps
	go build -o readout ./cmd/readout
	cd tests/e2e && READOUT_BIN=$(CURDIR)/readout RO_VISUAL=1 npx playwright test --project=visual --update-snapshots

# e2e-docker-preflight: the two fail-early gates shared by every containerized
# e2e target -- the image/package-pin agreement and the Docker VM memory floor.
.PHONY: e2e-docker-preflight e2e-docker-binaries
e2e-docker-preflight:
	@image_tag=$$(printf '%s' '$(PLAYWRIGHT_IMAGE)' | sed -E 's/.*:v([0-9.]+)-.*/\1/'); \
		pkg_ver=$$(node -p "require('./tests/e2e/package.json').devDependencies['@playwright/test']"); \
		if [ "$$image_tag" != "$$pkg_ver" ]; then \
			echo "ERROR: PLAYWRIGHT_IMAGE pins v$$image_tag but tests/e2e/package.json wants @playwright/test $$pkg_ver -- bump them together."; \
			exit 1; \
		fi
	# Emulated (Rosetta) Chromium OOMs below ~6 GiB; a 2 GiB VM gets SIGKILLed
	# (exit 137) at browser launch. Fail early with a fixable message instead.
	@mem=$$(docker info --format '{{.MemTotal}}'); \
		min=6442450944; \
		if [ "$$mem" -lt "$$min" ]; then \
			echo "ERROR: Docker VM has $$mem bytes; raise your Docker VM memory to >=6 GB (emulated Chromium OOMs below that)."; \
			exit 1; \
		fi

# e2e-docker-binaries: cross-compile the readout + harness binaries the image
# mounts in (the Playwright image has no Go toolchain). CGO_ENABLED=0 -> static
# linux/amd64; HARNESS_BIN/READOUT_BIN point the harness at them.
e2e-docker-binaries:
	mkdir -p .build/linux-amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o .build/linux-amd64/readout ./cmd/readout
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o .build/linux-amd64/harness ./tests/e2e/harness

## e2e-deps: install the e2e suite's npm deps and Chromium (both steps are idempotent)
e2e-deps:
	cd tests/e2e && npm install --no-audit --no-fund
	@if [ -n "$${PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH:-}" ]; then \
		test -x "$${PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH}" || { echo "ERROR: PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH is not executable: $${PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH}"; exit 1; }; \
		echo "using system Chromium: $${PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH}"; \
	else \
		cd tests/e2e && npx playwright install --with-deps chromium; \
	fi

## air: local live-reload dev server (dev-only; not a CI gate)
air:
	air

## help: list documented targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
