# Contributing

## Build & test

This repository pins its local toolchain with [mise](https://mise.jdx.dev/).
Bootstrap the tools once, then run the normal gates through the mise environment:

```sh
mise run setup
mise exec -- make ci
```

`mise run setup` installs Go, Node, Python, Gremlins, GNU Make, Zig (as the
cgo compiler for `go test -race`), golangci-lint, Helm, and kubeconform. It
puts the Go helper binaries used by the gates (`templ` and `govulncheck`) in
a repo-local ignored directory. `make ci` runs the local Go fast path (templ freshness, lint, comment
hygiene, mutation-harness guards, and the race test suite); GitHub CI
additionally runs vet, `govulncheck`, frontend, Playwright, and chart jobs. Run the relevant local gates
before sending a patch. Use `mise run doctor` to print the active tool versions
when debugging local setup.

The frontend has a separate fast gate:

```sh
mise exec -- make frontend-check
```

It lints and typechecks both production and test TypeScript, runs Vitest with
the enforced V8 coverage floor, rebuilds the embedded assets, and fails if the
committed bundle is stale. Use `npm run test:watch` for the local Vitest loop.

For a slower test-strength audit, first validate the Stryker sandbox and then
run the complete mutation gate:

```sh
mise exec -- npm run test:mutation:dry
mise exec -- npm run test:mutation:full
```

The full run includes static/load-time code and must finish with no surviving,
uncovered, timed-out, runtime-error, ignored, or pending mutants. Reports stay
local under `reports/mutation/`. This is intentionally a local test-strength
audit, not part of the ordinary CI gate: a complete run takes roughly 16 minutes
on the reference development machine.

The launcher accepts only an explicit dry/full mode plus concurrency 1 or 2; it
does not forward arbitrary Stryker CLI options. Under the mutation lock it
copies the exact SHA-256 input snapshot into `/.mutation-stage/`, links only
that stage's `node_modules` to the installed dependency tree, verifies that the
mutation candidates equal the shipped esbuild runtime graph, and runs Stryker
with the stage as its working directory. This keeps transient edits to the
working tree from entering Stryker's own sandbox. A full attempt invalidates the
previous JSON/HTML report first; Stryker writes its candidates inside the stage,
and the launcher publishes each report by atomic rename only after a successful
run, final input checks, complete process-group exit, and stage cleanup.

The success attestation records SHA-256 digests for both reports and the exact
staged input-set digest, plus a run window used to verify both files' freshness,
production TypeScript, every Vitest test and setup file, shared runtime test
data (including the prefs golden JSON corpus), `.mise.toml`, the Stryker,
Vitest, and TypeScript configs, the shared production bundle recipe, embedded
`readout.js`, the compatibility hook, package lock, and the launcher/checker
implementation. The proof also binds the exact Node version, OS platform, and
CPU architecture. The launcher and checker require that Node equal the exact
`.mise.toml` pin and satisfy `package.json`'s engine range.
`test:mutation:check` rejects a report after any of those inputs or runtime
properties changes, or while a stage/lock/recovery claim remains. A dry run
uses only the text reporter, cleans its stage, and cannot replace the full-run
proof.

The process-group resource guard supports POSIX macOS with BSD `ps`/`du` and
Linux hosts with procps/coreutils-compatible commands. It resolves both tools
through safe absolute `PATH` entries, ignoring relative paths and repository
descendants, then probes the required `ps -axo ...` and `du -sk` forms before
publishing a lock. It is not a Windows or BusyBox portability layer. Disk-free,
generated-data, and aggregate process-group RSS limits are userspace samples
(every 15 seconds by default), not kernel quotas: usage can exceed a threshold
between samples and while the group is terminating. Generated-data accounting
includes the complete stage and the published-report directory, including the
initial staged input copy. The runtime limit is a launcher timer, and RSS means
the resident memory reported for processes still in the guarded POSIX process
group. Use the `MUTATION_*` environment variables only to make the defaults
stricter on a smaller machine. A persisted child PGID lets the next launcher
verify, stop, and wait for an orphaned group before it removes the deterministic
stage and legacy temp directory. Recovery uses an atomic fixed hard-link claim,
so concurrent launchers cannot delete a newly published owner; an abandoned
claim or unverifiable stale lock is retained for manual inspection. If launcher
IPC disappears, the detached child terminates its whole process group so no
Stryker worker can continue without the resource monitor.

Go mutation testing is also a deliberate local audit, not a full CI job. After
`mise run setup`, the next campaign starts with one command:

```sh
mise run mutation-go
```

It resumes only input-matching package results and prints unresolved mutants.
Use `mise run mutation-go-full` for a fresh all-package proof, followed by
`mise run mutation-go-check`; that check allows no living, uncovered, or
timed-out build-valid mutants. The Go shim runs a non-executing build/vet
preflight for each mutant, records confirmed source diagnostics as NOT VIABLE,
and aborts on unrecognized infrastructure failures. It accepts KILLED only when
the preflight passed before the real test command failed. Every run first proves Gremlins with a real
known-killed/known-lived/build-invalid canary and uses re-verified staged source
plus isolated, bounded Go build/module/temp directories; it never cleans the
normal user Go cache. CI runs only the fast harness/scope guard. Narrow-package,
sanity, cleanup, report, and resource-limit details are in
[tools/mutation/README.md](tools/mutation/README.md).

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
