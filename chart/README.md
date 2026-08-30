# readout

A Helm chart for **readout**, a strictly read-only Kubernetes web viewer written
in Go. The chart deploys readout, the cluster read access it needs (RBAC,
read-only by construction), and the routing/exposure objects of your choice
(Ingress, Gateway API, or a controller CR via the escape hatch).

readout serves on its own host (a subdomain or a root domain) — it does **not**
support subpath deployment, and `publicUrl` is validated as origin-only (scheme
+ host, no path).

## Install

The chart is published as an OCI artifact:

```sh
helm install readout oci://ghcr.io/kbelokon/charts/readout --version 0.12.0
```

Or, with your own values:

```sh
helm upgrade --install readout oci://ghcr.io/kbelokon/charts/readout \
  --version 0.12.0 -f my-values.yaml
```

The smallest honest install (single replica, no auth, no exposure — reach it
with `kubectl port-forward`) is in [`examples/minimal.yaml`](examples/minimal.yaml).

The image tag defaults to the chart's `appVersion`. Set `image.tag` to override,
or `image.digest` to pin by digest — a digest always wins and the tag is ignored.

## Upgrading from ≤ 0.6 (breaking)

Chart 0.7 changes the resource **identity**. Pod/selector labels moved from the
legacy `application: readout` to the standard
`app.kubernetes.io/name` + `app.kubernetes.io/instance`, and rendered object
names are now release-scoped. Kubernetes forbids mutating a Deployment's
`spec.selector` and a Service's selector in place, so `helm upgrade` from a ≤ 0.6
release fails.

readout is a **stateless** application — it holds no persistent data — so the
migration is a clean reinstall with **no data loss**:

```sh
helm uninstall readout
helm install readout oci://ghcr.io/kbelokon/charts/readout --version 0.12.0 -f my-values.yaml
```

Fresh installs of 0.7 are unaffected.

## Upgrading from ≤ 0.12 with a NetworkPolicy (behaviour change)

`networkPolicy.ingress.from` now opens **only the app port** (`config.port`).
Earlier charts also opened the metrics port (when `metrics.enabled`) to every
`from` peer, which handed the (possibly unauthenticated) dashboard to your
scraper and the metrics port to your ingress controller. Move scraper peers to
the new `networkPolicy.ingress.metricsFrom`; until you do, Prometheus loses the
scrape on an enforcing CNI. See [NetworkPolicy](#networkpolicy).

Unrelated to the policy but in the same release: `config.listenAddress` now
defaults to `"0.0.0.0"`. Installs on `config.auth.mode: none` that never became
Ready (the app bound loopback) start working; nothing changes for `oidc` /
`headers` installs.

## Values reference

Every public key in `values.yaml`. Nested keys are described in the parent row.

| Key | Default | Description |
| --- | --- | --- |
| `image` | `{repository: ghcr.io/kbelokon/readout, tag: "", digest: "", pullPolicy: IfNotPresent}` | Container image. `tag` defaults to the chart `appVersion` when empty; `digest`, when set, wins over `tag`. |
| `nameOverride` | `""` | Override the chart name used in generated resource names. |
| `fullnameOverride` | `""` | Override the fully-qualified release name outright. |
| `commonLabels` | `{}` | Labels merged into every rendered resource (applied last, so they override standard chart labels of the same name). |
| `commonAnnotations` | `{}` | Annotations merged into every rendered resource. Per-resource annotation blocks win on conflict; not part of the pod config checksum, so changing it does not roll pods. |
| `serviceAccount` | `{create: true, name: "", annotations: {}}` | The ServiceAccount readout runs as. When `create` is false, set `name` to an existing account; binding it to roles is then your responsibility. |
| `rbac` | `{create: true, preset: restricted, extraRules: []}` | Cluster read access. Defaults to the least-privilege `restricted` preset. See [RBAC presets](#rbac-presets) below. When `create` is false, no RBAC objects render. `extraRules` are appended verbatim (keep verbs within get/list/watch — schema-pinned). |
| `replicaCount` | `1` | Number of readout pod replicas. |
| `service` | `{type: ClusterIP, port: 80, labels: {}, annotations: {}}` | The main Service. `labels`/`annotations` merge over the common sets. |
| `metrics` | `{enabled: false, port: 9090, service: {...}, serviceMonitor: {...}}` | Separate metrics listener. When enabled, renders `config.metricsPort = metrics.port`, adds a `metrics` container port, a `<fullname>-metrics` Service, and an optional `serviceMonitor` (Prometheus Operator CRD). See the metrics guards below. |
| `ingress` | `{enabled: false, className: "", annotations: {}, hosts: [], tls: []}` | Expose readout through a `networking.k8s.io/v1` Ingress. At least one `hosts` entry is required when enabled; `tls` passes through verbatim. |
| `gateway` | `{enabled: false, apiVersion: gateway.networking.k8s.io/v1, parentRefs: [], hostnames: [], annotations: {}, rules: []}` | Expose readout by attaching an HTTPRoute to an existing Gateway. `parentRefs` is required when enabled. The chart never creates a Gateway or GatewayClass. |
| `config` | `{port: 8080, listenAddress: "0.0.0.0", excludeNamespaces: [kube-.*], showContainerLogs: false, includeSecrets: false, auth: {mode: none}}` | The readout app config, serialized verbatim into a ConfigMap as `readout.yaml` and mounted at `/etc/readout/readout.yaml`. Holds no secrets — those come from `env`/`envFrom`. Optional `argoCD` and `auth.trustedHeaders`/`auth.oidc` blocks live here too. `listenAddress` is set explicitly because the app's default for an empty value under `auth.mode: none` is loopback — unreachable by probes and the Service inside a pod; change it only to `127.0.0.1` behind an in-pod sidecar proxy. |
| `config.live` | app defaults `{maxConnections: 512, maxSources: 128, maxCacheAccountedBytes: 128Mi}` (commented out in `values.yaml`) | Per-pod bounds on the Live (SSE) surface: open Live streams, distinct upstream LIST+watch sources, and accounted retained object bytes. Browsers on the same scope and credentials share one source, so `maxSources` sits far below `maxConnections` — but under `config.clusterAuthUseSessionToken` each viewer's token is its own source, so size `maxSources` against concurrent viewers instead. Each pod enforces its own copy, so with `replicaCount > 1` the deployment-wide ceiling is that many times each value; a refused subscriber gets `429` with `Retry-After` and the page keeps working through Refresh. Lower `maxCacheAccountedBytes` when `resources.limits.memory` is below the chart default. |
| `auth` | `{sessionSecret: {existingSecret: "", key: session-secret}, oidc: {existingSecret: "", clientIdKey: client-id, clientSecretKey: client-secret}}` | Typed secret wiring. Point at Secrets you already created; the chart renders matching `READOUT_*` env via `secretKeyRef`. Empty `existingSecret` disables a wiring. See [Secret wiring](#secret-wiring). |
| `env` | `[]` | Literal extra `READOUT_*` env entries. Rendered after the typed `auth` entries, so an `env` entry of the same name wins. |
| `envFrom` | `[]` | `envFrom` references to existing Secrets/ConfigMaps (e.g. a Secret holding `READOUT_SESSION_SECRET`). Opaque to the chart's gates. |
| `resources` | `{requests: {cpu: 50m, memory: 128Mi}, limits: {cpu: 500m, memory: 512Mi}}` | Container resource requests/limits. Real defaults, not empty: an unbounded container is a DoS surface — one expensive list-and-render request (e.g. a namespace with thousands of objects, or a client looping such requests) drives CPU/heap until OOM. The limit caps the blast radius; the request reserves scheduler room. Tune for your cluster. |
| `automountServiceAccountToken` | `true` | Explicit (not Kubernetes' silent default): readout reaches the apiserver with the SA token in Live/in-cluster mode, so it must be mounted. Set false only in a mode that never talks to the apiserver (then drop `rbac.create` too). |
| `networkPolicy` | `{enabled: false, ingress: {from: [], metricsFrom: []}, egress: {dns: true, to: []}}` | Opt-in `networking.k8s.io/v1` NetworkPolicy bounding readout's ingress/egress. Off by default. `ingress.from` opens the app port only, `ingress.metricsFrom` the metrics port only. See [NetworkPolicy](#networkpolicy) — **enforced only by a CNI that implements NetworkPolicy.** |
| `podSecurityContext` | `{runAsNonRoot: true, seccompProfile: {type: RuntimeDefault}}` | Pod-level security context. |
| `securityContext` | `{allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}` | Container-level security context. |
| `podAnnotations` | `{}` | Annotations added to the pod template only. |
| `podLabels` | `{}` | Labels added to the pod template only. |
| `nodeSelector` | `{}` | Pod `nodeSelector` (rendered only when set). |
| `tolerations` | `[]` | Pod tolerations (rendered only when set). |
| `affinity` | `{}` | Pod affinity (rendered only when set). |
| `topologySpreadConstraints` | `[]` | Pod topology spread constraints (rendered only when set). |
| `priorityClassName` | `""` | Pod `priorityClassName` (rendered only when set). |
| `extraVolumes` | `[]` | Extra volumes added to the pod. |
| `extraVolumeMounts` | `[]` | Extra volume mounts appended to the readout container. |
| `initContainers` | `[]` | Extra init containers, rendered verbatim into the pod. |
| `extraContainers` | `[]` | Extra sidecar containers, rendered verbatim into the pod. |
| `podDisruptionBudget` | `{enabled: false, minAvailable: "", maxUnavailable: ""}` | PodDisruptionBudget for readout pods. Set exactly one of `minAvailable`/`maxUnavailable` (Kubernetes rejects both). |
| `extraObjects` | `[]` | Escape hatch for arbitrary Helm-owned objects (platform CRs, extra Secrets). Each entry is one YAML map; string values run through `tpl` with the chart root context. See [Exposure recipes](#exposure-recipes). |
| `testFramework` | `{enabled: false, image: {repository: curlimages/curl, tag: "8.11.1"}}` | Opt-in `helm test` connectivity pod. When enabled, `helm test <release>` runs a curl pod against the Service's `/readyz`. The pod carries no `app.kubernetes.io/name`, so it is outside every chart selector; under `networkPolicy.enabled` the policy admits it automatically. |

## Exposure recipes

readout always serves on its own host. Pick one routing path.

### Ingress

Set `ingress.enabled: true`, a `className`, and at least one host. Full recipe in
[`examples/ingress-nginx.yaml`](examples/ingress-nginx.yaml):

```yaml
ingress:
  enabled: true
  className: nginx
  hosts:
    - host: readout.example.com
      paths:
        - path: /
          pathType: Prefix
```

### Gateway API

Set `gateway.enabled: true` and at least one `parentRefs` entry (an HTTPRoute
with no parent is invalid, so the template fails fast otherwise). The chart never
creates the Gateway or GatewayClass. Full recipe in
[`examples/gateway-api.yaml`](examples/gateway-api.yaml):

```yaml
gateway:
  enabled: true
  parentRefs:
    - name: public-gateway
      namespace: gateway-system
      sectionName: https
  hostnames:
    - readout.example.com
```

### Controller CR via extraObjects

When your routing layer is neither a stock Ingress nor a Gateway API HTTPRoute
(for example a Traefik `IngressRoute`), ship it through `extraObjects`. String
values run through `tpl`, so they can reference chart helpers like
`{{ include "readout.fullname" $ }}`. Full recipe in
[`examples/extra-objects.yaml`](examples/extra-objects.yaml).

## RBAC presets

readout's access is read-only by construction: this chart never grants a mutating
verb on any resource. Verbs are always `get`/`list`/`watch`.

- **`restricted`** (default) — least privilege: grants are **derived from
  `config`** (the single source of truth). Secrets read and `pods/log` read are
  granted only when `config.includeSecrets` / `config.showContainerLogs` ask for
  them; the Argo CD namespaced Role/RoleBinding renders only when `config.argoCD`
  reads cluster Secrets from the in-cluster ServiceAccount.
- **`wildcard`** (loud opt-in) — `get`/`list`/`watch` on every apiGroup and
  resource, including any CRD the cluster serves **and including API-level read on
  Secrets even when `config.includeSecrets` is false**. This makes "discover
  everything" work out of the box, at the cost of handing the ServiceAccount token
  apiserver-direct read on every Secret in the cluster.

**Wildcard trade-off:** readout's own app-level gate still refuses to admit the
Secret type when `config.includeSecrets` is false, but a compromise of the
ServiceAccount token bypasses that gate and exposes every Secret in the cluster
through the apiserver directly. The default is `restricted`; select `wildcard`
only when that exposure is acceptable. (Documented trade-off — not a bug.)

Widen either preset with `rbac.extraRules`, appended verbatim. Keep verbs within
`get`/`list`/`watch` — the schema pins this list to read-only verbs.

## Safety gates and the env boundary

The chart **never refuses to render over a security or operational posture**: a
valid-but-risky choice (no-auth exposure, multi-replica OIDC without a shared
session secret) installs and prints a loud NOTES warning. The only render-time
`fail`s are for **combinations the Kubernetes API itself rejects, or that would
silently break the release** (a listener that cannot bind, a rule that cannot
render) — failing early with a clear message beats a cryptic apply error or a
quietly missing feature. Every gate sees **chart values only**: config
delivered through `env`/`envFrom` (opaque references the chart cannot read)
bypasses them entirely, backstopped by the **app's own startup checks** and
`readout config validate` (see [Validating before install](#validating-before-install)).

Warnings (install proceeds, NOTES warns):

- **No-auth exposure** — exposing readout while `config.auth.mode` is `none`
  (through Ingress, Gateway, or a `LoadBalancer`/`NodePort` Service) is **never
  render-blocked**: the chart installs and prints a loud NOTES warning instead.
  An exposed no-auth instance publishes an unauthenticated, cluster-wide read
  viewer; set a real `auth.mode` (`oidc`/`headers`) before trusting the exposure.
- **Multi-replica OIDC session secret** — `config.auth.mode: oidc` with
  `replicaCount > 1` and no chart-visible session secret installs but prints a
  loud NOTES warning (each replica signs with its own ephemeral key, so OIDC
  login breaks under load balancing unless you have sticky sessions). Wire a
  shared secret via `auth.sessionSecret.existingSecret`, an `env` entry named
  `READOUT_SESSION_SECRET`, or `config.sessionSecretFile`; an opaque `envFrom`
  source is assumed to carry it and the warning softens to a reminder.

Render-time `fail`s (the cluster would reject these anyway, or the release would
be silently broken):

- **PodDisruptionBudget** — `minAvailable` and `maxUnavailable` both set is
  rejected by the PDB API; the chart fails early naming the conflict.
- **Identity-label collision** — overriding `app.kubernetes.io/name` or
  `app.kubernetes.io/instance` via `commonLabels`/`podLabels` produces an
  immutable-selector mismatch the API rejects; the chart fails early naming the
  key.
- **Metrics guards** — `config.metricsPort` set to a value different from
  `metrics.port` is an error; `config.metricsPort` set while `metrics.enabled` is
  false is an error; `metrics.port` equal to `config.port` is an error (the two
  listeners would fight for one port: the pod crash-loops or `/metrics` silently
  vanishes); a `serviceMonitor` enabled without `metrics.enabled` does not render
  a useful object. Drive the metrics port through `metrics.port` only.
- **NetworkPolicy guard** — `networkPolicy.ingress.metricsFrom` set while
  `networkPolicy.enabled` is true and `metrics.enabled` is false is an error:
  there is no metrics port to open, and a silently missing rule would leave the
  operator believing the scraper is admitted.
- **Live capacity is not a render gate** — the `config.live` bounds are enforced
  at runtime by each pod, not by the chart; watch the per-pod `readout_live_*` /
  `readout_watchhub_*` families on the metrics listener for utilization and
  admission rejects.

## Secret wiring

The chart **never creates Secret objects** and holds no secret values. Create
your Secrets first, then wire them by reference. Typed wiring (recommended):

```sh
kubectl create secret generic readout-session \
  --from-literal=session-secret="$(openssl rand -hex 32)"
kubectl create secret generic readout-oidc \
  --from-literal=client-id=YOUR_OIDC_CLIENT_ID \
  --from-literal=client-secret=YOUR_OIDC_CLIENT_SECRET
```

```yaml
auth:
  sessionSecret:
    existingSecret: readout-session
    key: session-secret
  oidc:
    existingSecret: readout-oidc
    clientIdKey: client-id
    clientSecretKey: client-secret
```

This renders `READOUT_SESSION_SECRET`, `READOUT_OIDC_CLIENT_ID`, and
`READOUT_OIDC_CLIENT_SECRET` via `secretKeyRef`. For anything the typed surface
does not cover, use `env` (literal `valueFrom`) or `envFrom` (whole-Secret
reference). Full recipe in
[`examples/oidc-existing-secret.yaml`](examples/oidc-existing-secret.yaml).

## Own-host law

readout serves on its own host (a subdomain or a root domain); subpath
deployment is unsupported and `publicUrl` is validated as origin-only.

## NetworkPolicy

`networkPolicy.enabled: true` renders an opt-in `networking.k8s.io/v1`
NetworkPolicy that bounds readout's blast radius: a default-deny baseline plus
only the ingress/egress you explicitly allow.

> **CNI caveat.** A NetworkPolicy is enforced **only** by a CNI that implements
> it (Calico, Cilium, Antrea, …). On a cluster whose CNI ignores NetworkPolicy
> (e.g. plain flannel) this object applies cleanly but enforces **nothing** — it
> is not a security control there. Verify your CNI before relying on it.

- **Ingress** — two peer lists, each opening exactly **one** port, so a peer
  never receives a port it was not listed for:
  `networkPolicy.ingress.from` (verbatim `NetworkPolicyPeer`s) reaches the app
  port `config.port` **only** — your ingress controller / reverse proxy / auth
  proxy; `networkPolicy.ingress.metricsFrom` reaches the metrics port
  `metrics.port` **only** — your Prometheus / vmagent (requires
  `metrics.enabled`; the chart fails the render otherwise). **Both left empty,
  ingress is default-deny** — nothing reaches readout (apart from the
  `helm test` rule below).
- **`helm test` under the policy** — with `testFramework.enabled` the policy adds
  one more rule admitting the test pod (by `app.kubernetes.io/instance` +
  `app.kubernetes.io/component: test-connection`, this namespace only) to the
  app port. The test pod carries no `app.kubernetes.io/name`, so it is not a
  target of the policy itself.
- **Egress** — `networkPolicy.egress.dns` (default `true`) allows DNS on UDP/TCP
  53. `networkPolicy.egress.to` is a list of verbatim `NetworkPolicyEgressRule`s
  for **every** destination readout dials: the in-cluster **Kubernetes
  apiserver**; each **remote apiserver** configured through
  `config.kubeconfigPath`, `config.clusters` or Argo CD cluster Secrets; your
  **OIDC issuer**; and the **hook endpoints** (`config.hooks.authorizationUrl` /
  `resourcePrerenderUrl`). Left empty, readout can reach only DNS — every
  cluster view and OIDC login breaks — so scope these to your endpoints.

```yaml
metrics:
  enabled: true
networkPolicy:
  enabled: true
  ingress:
    from:                          # app port only
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: ingress-nginx
    metricsFrom:                   # metrics port only
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: monitoring
  egress:
    dns: true
    to:
      - to:
          - ipBlock:
              cidr: 10.0.0.1/32   # in-cluster apiserver
        ports:
          - protocol: TCP
            port: 443
      - to:
          - ipBlock:
              cidr: 0.0.0.0/0     # OIDC issuer, remote apiservers, hooks (pin tighter if you can)
        ports:
          - protocol: TCP
            port: 443
```

## Validating before install

Two complementary checks:

- **Chart values** — render the manifests and let the gates run:

  ```sh
  helm template readout oci://ghcr.io/kbelokon/charts/readout --version 0.12.0 -f my-values.yaml
  ```

- **Raw app config** — validate a `readout.yaml` exactly as startup would:

  ```sh
  readout config validate --config readout.yaml
  ```

  Exits `0` and prints `config OK`, or exits `1` with the same error startup
  would print (exit `2` on usage error). **Caveat:** it is faithful to a real
  startup, so `READOUT_*` environment variables in your shell affect the result
  (env overrides the file). Validate in the environment the process will actually
  run in.

## Examples

Eight runnable values files in [`examples/`](examples):

| File | What it deploys |
| --- | --- |
| [`minimal.yaml`](examples/minimal.yaml) | Smallest honest install: single ClusterIP pod, restricted preset (default), auth none, no exposure (reach via port-forward). |
| [`kubeconfig-multicluster.yaml`](examples/kubeconfig-multicluster.yaml) | A CI/Vault-rendered multi-context kubeconfig mounted from a Secret via `extraVolumes`, with `config.kubeconfigPath` pointing at the file so every context becomes a cluster. Allowlisted cloud exec plugins (aws, gke-gcloud-auth-plugin, ...) survive the default kubeconfig-source policy. |
| [`ingress-nginx.yaml`](examples/ingress-nginx.yaml) | readout behind a `networking.k8s.io/v1` Ingress on the nginx class. |
| [`gateway-api.yaml`](examples/gateway-api.yaml) | readout exposed by attaching an HTTPRoute to an existing Gateway API Gateway. |
| [`oidc-existing-secret.yaml`](examples/oidc-existing-secret.yaml) | OIDC login behind an Ingress, with session and client Secrets wired from pre-created Secrets. The recommended exposed setup. |
| [`metrics-servicemonitor.yaml`](examples/metrics-servicemonitor.yaml) | Separate metrics listener plus a Prometheus Operator ServiceMonitor. |
| [`argocd-cluster-secrets.yaml`](examples/argocd-cluster-secrets.yaml) | Argo CD cluster-Secret discovery with the least-privilege restricted preset. |
| [`extra-objects.yaml`](examples/extra-objects.yaml) | A platform CR (Traefik IngressRoute) shipped through the extraObjects escape hatch. |
