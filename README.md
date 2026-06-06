# readout

A Go reimplementation / spiritual successor of [kube-web-view](https://codeberg.org/hjacobs/kube-web-view)
by Henning Jacobs (GitHub mirror: <https://github.com/hjacobs/kube-web-view>).

readout is a strictly **read-only**, multi-cluster, any-resource Kubernetes
viewer. It discovers built-in resources and arbitrary CRDs at runtime, renders
them as server-side `meta.k8s.io` Tables, and serves them over a read-only HTTP
edge — no write verbs, no `kubectl apply`, nothing that mutates a cluster. Point
it at one cluster or many; browse list / detail / YAML / events / container logs;
search across resource types; and follow operator-defined links out to your own
dashboards, wikis, and log systems.

## Run

```sh
readout --config readout.yaml
```

That is the whole command line. readout has exactly four flags — everything else
about behavior lives in the YAML config:

| Flag        | Purpose                                              |
| ----------- | ---------------------------------------------------- |
| `--config`  | path to the YAML config file (see below)             |
| `--port`    | TCP listen port (overrides `port:` in the config)    |
| `--debug`   | verbose logging                                      |
| `--version` | print the version and exit                           |

A documented, copy-pasteable example config lives at
[`readout.yaml`](readout.yaml). Start from it.

## Configuration

Everything that used to be a flag is a field in `readout.yaml`, parsed with
`sigs.k8s.io/yaml` (so it is the JSON-subset of YAML: standard scalars, lists,
and maps; no anchors or merge keys). Unknown keys are rejected, so a typo fails
fast at startup. The customization surface, all in the YAML:

- **clusters** — statically configured cluster connections using kubeconfig field
  semantics (`server`, `certificateAuthority`/`certificateAuthorityData`,
  `tlsServerName`, `token`/`tokenFile`, client cert/key, `impersonate`), and/or
  kubeconfig discovery (`kubeconfigPath` / `kubeconfigContexts`). See
  [Connecting to clusters](#connecting-to-clusters) below.
- **columns** — per resource type: `labelColumns` (promote a label to a column),
  `hiddenColumns` (drop a column), and `customColumns`. Custom columns are
  **kubectl-style JSONPath** expressions in `NAME:path` form, exactly like
  `kubectl get -o custom-columns`. A bare path and a braced template are both
  accepted, e.g. `Image:{.spec.containers[*].image}`. Multi-value results render
  space-joined.
- **links** — external links rendered next to objects (`objectLinks`, keyed by
  resource-type plural), label values (`labelLinks`, keyed by label name), and
  timestamps (`timestampLinks`). Each `href` template expands `{name}`,
  `{namespace}`, `{value}`, and `{timestamp}` at render time.
- **sidebar** — an **ordered** list of navigation groups; groups render
  top-to-bottom in the order written, each a heading `label` plus its
  `resources`. Omit it to use the built-in default layout.
- **preferredApiVersions** — pin a preferred `apiVersion` per resource-type
  plural when several versions are served.
- **search** — `defaultResourceTypes`, `offeredResourceTypes`, and
  `maxConcurrency` for multi-resource search.
- **namespaces** — `includeNamespaces` / `excludeNamespaces` RE2 regular
  expressions (exclude wins; empty include means all; cluster-scoped objects are
  never namespace-excluded).

See [`readout.yaml`](readout.yaml) for the full annotated schema, including auth
(`none` / `headers` / `oidc`), theming, external readout cross-links, and the
external JSON HTTP hooks.

### Connecting to clusters

A cluster connection is built from kubeconfig field semantics and handed to
client-go, so readout produces TLS and auth exactly the way `kubectl` does —
nothing is hand-rolled. There are two sources, which may be combined:

- **Static** — entries under `clusters:`, each a per-cluster block with
  kubeconfig field names that map 1:1 onto a kubeconfig cluster + user:
  - **Endpoint & TLS** — `server`; CA trust via `certificateAuthority` (PEM file)
    or `certificateAuthorityData` (inline, base64); `tlsServerName` to override
    the verified hostname when `server` is an IP or differs from the cert SAN;
    `insecureSkipTlsVerify` to skip verification (avoid in production — pin the CA
    instead).
  - **Auth** — an inline `token`, or a `tokenFile` that client-go re-reads on
    rotation (prefer `tokenFile` for anything that rotates; an inline `token`
    shadows the file and disables refresh), or client-certificate mTLS via
    `clientCertificate`/`clientKey` (PEM files) or
    `clientCertificateData`/`clientKeyData` (inline, base64).
  - **Identity** — `impersonate: {user, groups, uid}` sets a static act-as
    identity for that cluster's base connection.
  - Cluster names must be unique; a duplicate name is a startup error.
- **kubeconfig** — `kubeconfigPath` (empty uses the usual kubeconfig resolution)
  and `kubeconfigContexts` (narrow to named contexts; empty = all).

#### Viewer identity (token passthrough)

By default a connection is used as configured (its static token / cert /
`impersonate`). Set `clusterAuthUseSessionToken: true` to instead forward the
**viewer's own** session token to the apiserver per request, so every request is
evaluated under the viewer's RBAC. Passthrough takes precedence for that request:
the connection's static token **and** `impersonate` are dropped, so a passthrough
request is always evaluated as the viewer, never as the static act-as identity.

#### `exec` credential plugins (binary prerequisite)

An `exec`-style credential plugin (the kubeconfig `users[].user.exec` mechanism
used by `aws eks get-token`, `gke-gcloud-auth-plugin`, `kubelogin`, etc.) is a
supported auth field on a connection, but **readout's image does not bundle these
plugin binaries** — same posture as Headlamp, whose image ships only
`ca-certificates`. The connection is configured for you; the plugin binary is the
operator's prerequisite and must be present on `PATH` in readout's runtime image,
or the cluster fails at connect time. Getting those binaries onto the container
without forking the image (an init-container + shared `emptyDir` on `PATH`, or a
native image volume) is tracked as backlog **B-002**
([`docs/forge/backlog.md`](docs/forge/backlog.md)).

### Secrets (environment only)

Secrets are never written to the config file — they come from the environment,
and the environment **overrides** the file:

| Variable                              | Purpose                          |
| ------------------------------------- | -------------------------------- |
| `READOUT_SESSION_SECRET`              | signing key for session cookies  |
| `READOUT_OIDC_CLIENT_ID`              | OIDC client id                   |
| `READOUT_OIDC_CLIENT_SECRET`          | OIDC client secret               |
| `READOUT_OIDC_ISSUER_URL`             | OIDC issuer URL                  |
| `READOUT_OIDC_REDIRECT_URL`           | OIDC redirect URL                |
| `READOUT_AUTHORIZATION_HOOK_URL`      | authorization hook URL           |
| `READOUT_RESOURCE_PRERENDER_HOOK_URL` | resource-prerender hook URL      |

## Endpoints

- read-only HTTP edge; the only state-changing route is the allowlisted
  `POST /preferences` (theme preference);
- resource list / detail / YAML / events / search / container-logs / download
  routes, plus `_all` cluster and `_all` namespace fan-out;
- `join=metrics` and `join=nodes` enrichment for Pods and Nodes;
- the `Secret` type is dropped by default and re-admitted (with masked values)
  only when explicitly included;
- `/health`, `/healthz`, `/readyz`, and Prometheus `/metrics`.

## Metrics

Prometheus metrics exposed at `/metrics`:

| Metric                                  | Type      | Meaning                              |
| --------------------------------------- | --------- | ------------------------------------ |
| `readout_http_requests_total`           | counter   | HTTP requests served                 |
| `readout_http_request_duration_seconds` | histogram | HTTP request latency                 |
| `readout_up`                            | gauge     | process liveness (1 while serving)   |

## Build

Build the binary from source:

```sh
go build ./cmd/readout
```

Or install it directly:

```sh
go install github.com/kbelokon/readout/cmd/readout@latest
```

## Install

### Container image (multi-arch: amd64 + arm64)

```sh
docker run --rm -p 8080:8080 -v "$PWD/readout.yaml:/readout.yaml" ghcr.io/kbelokon/readout:latest --config /readout.yaml
```

### Helm chart (OCI, from GHCR)

```sh
helm install readout oci://ghcr.io/kbelokon/charts/readout --version 0.3.0
```

## Deploy

A kustomize base also lives in [`deploy/kustomize`](deploy/kustomize). The
chart's `image.tag` / `appVersion` track the app release they target; the
chart's own version moves independently.

## License & attribution

readout is licensed under the **GNU GPL-3.0** ([`LICENSE`](LICENSE)) — it is a
derivative of kube-web-view and honors its copyleft. Upstream and bundled
third-party attribution is in [`NOTICE`](NOTICE).
