# Contributing

## Build & test

```sh
make ci
```

`make ci` runs the required gates (templ freshness, lint, and the race test
suite) — the same gates CI enforces. Run it before sending a patch.

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
