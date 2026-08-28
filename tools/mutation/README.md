# Go mutation testing

Bootstrap once:

    mise run setup

For the next mutation campaign, run:

    mise run mutation-go

That command validates the harness and Gremlins canary, reuses only results
whose code, tests, test-only dependencies, config, Go runtime, and Gremlins
binary still match, then prints every unresolved mutant. To deliberately
recompute every package and publish a fresh full proof:

    mise run mutation-go-full
    mise run mutation-go-check

Useful narrower commands:

    mise exec -- python3 tools/mutation/evaluate.py --package internal/web --explain
    mise run mutation-go-sanity
    mise run mutation-go-clean

mutation-go-clean removes only the harness-owned cache under the repository's
Git common directory. It never runs go clean against the user's normal Go
cache.

Full mutation testing is intentionally not a normal CI job. CI runs only the
fast harness guards. A local run stages Git-tracked plus non-ignored untracked
inputs, limits Gremlins to two workers, uses isolated temp/build-cache and
module/home/Zig-cache directories, enforces an input-size cap plus disk/RSS/
deadline limits, and stops the whole process group on failure or interruption.
The staged source is re-hashed before cache reuse and publication. Reports stay
ignored under reports/mutation/go/.

The strict check requires zero living, uncovered, or timed-out build-valid
mutants. Before every mutant test, the Go shim performs a non-executing
`go test -c` build/vet preflight. Confirmed in-source build diagnostics are
recorded and cross-checked as NOT VIABLE; any unrecognized infrastructure
failure aborts the shard. KILLED is accepted only after that preflight succeeds
and the real test command fails.

A resumable run writes latest-resume.json and cannot overwrite latest.json, the
fresh-full proof. An interrupted full attempt preserves the previous proof for
inspection but records an unfinished-attempt marker, so mutation-go-check stays
blocked until a new full run succeeds.

Do not add Gremlins v0.6.0's test-cpu option. That version passes -cpu 1 to go
test as one malformed argument and can misreport the resulting exit as a killed
mutant. The canary contains exact known-killed, known-lived, and build-invalid
mutants, so status drift fails closed. Ambient GOFLAGS, custom CGO flags,
workspace discovery, project/Kubernetes state, and repo-shadowed tool binaries
are rejected or isolated rather than entering the proof silently.
