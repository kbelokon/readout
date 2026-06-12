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
helm install readout oci://ghcr.io/kbelokon/charts/readout --version 0.8.0
```

Or, with your own values:

```sh
helm upgrade --install readout oci://ghcr.io/kbelokon/charts/readout \
  --version 0.8.0 -f my-values.yaml
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
helm install readout oci://ghcr.io/kbelokon/charts/readout --version 0.8.0 -f my-values.yaml
```

Fresh installs of 0.7 are unaffected.

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
| `rbac` | `{create: true, preset: wildcard, extraRules: []}` | Cluster read access. See [RBAC presets](#rbac-presets) below. When `create` is false, no RBAC objects render. `extraRules` are appended verbatim (keep verbs within get/list/watch — schema-pinned). |
| `replicaCount` | `1` | Number of readout pod replicas. |
| `service` | `{type: ClusterIP, port: 80, labels: {}, annotations: {}}` | The main Service. `labels`/`annotations` merge over the common sets. |
| `metrics` | `{enabled: false, port: 9090, service: {...}, serviceMonitor: {...}}` | Separate metrics listener. When enabled, renders `config.metricsPort = metrics.port`, adds a `metrics` container port, a `<fullname>-metrics` Service, and an optional `serviceMonitor` (Prometheus Operator CRD). See the metrics guards below. |
| `ingress` | `{enabled: false, className: "", annotations: {}, hosts: [], tls: []}` | Expose readout through a `networking.k8s.io/v1` Ingress. At least one `hosts` entry is required when enabled; `tls` passes through verbatim. |
| `gateway` | `{enabled: false, apiVersion: gateway.networking.k8s.io/v1, parentRefs: [], hostnames: [], annotations: {}, rules: []}` | Expose readout by attaching an HTTPRoute to an existing Gateway. `parentRefs` is required when enabled. The chart never creates a Gateway or GatewayClass. |
| `config` | `{port: 8080, excludeNamespaces: [kube-.*], showContainerLogs: false, includeSecrets: false, auth: {mode: none}}` | The readout app config, serialized verbatim into a ConfigMap as `readout.yaml` and mounted at `/etc/readout/readout.yaml`. Holds no secrets — those come from `env`/`envFrom`. Optional `argoCD` and `auth.headers`/`auth.oidc` blocks live here too. |
| `auth` | `{sessionSecret: {existingSecret: "", key: session-secret}, oidc: {existingSecret: "", clientIdKey: client-id, clientSecretKey: client-secret}}` | Typed secret wiring. Point at Secrets you already created; the chart renders matching `READOUT_*` env via `secretKeyRef`. Empty `existingSecret` disables a wiring. See [Secret wiring](#secret-wiring). |
| `env` | `[]` | Literal extra `READOUT_*` env entries. Rendered after the typed `auth` entries, so an `env` entry of the same name wins. |
| `envFrom` | `[]` | `envFrom` references to existing Secrets/ConfigMaps (e.g. a Secret holding `READOUT_SESSION_SECRET`). Opaque to the chart's gates. |
| `resources` | `{}` | Container resource requests/limits. |
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
| `unsafe` | `{allowNoAuth: false, allowEphemeralSessionSecret: false}` | Acknowledgements that silence the chart's safety gates. See [Safety gates and the env boundary](#safety-gates-and-the-env-boundary). |
| `testFramework` | `{enabled: false, image: {repository: curlimages/curl, tag: "8.11.1"}}` | Opt-in `helm test` connectivity pod. When enabled, `helm test <release>` runs a curl pod against the Service's `/readyz`. |

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

- **`wildcard`** (default) — `get`/`list`/`watch` on every apiGroup and resource,
  including any CRD the cluster serves. This is what makes "discover anything"
  work out of the box.
- **`restricted`** — least privilege: grants are **derived from `config`** (the
  single source of truth). Secrets read and `pods/log` read are granted only when
  `config.includeSecrets` / `config.showContainerLogs` ask for them; the Argo CD
  namespaced Role/RoleBinding renders only when `config.argoCD` reads cluster
  Secrets from the in-cluster ServiceAccount.

**Wildcard trade-off:** the wildcard preset grants API-level **read on Secrets**
even when `config.includeSecrets` is false. readout's own app-level gate still
refuses to admit the Secret type in that case, but a compromise of the
ServiceAccount token exposes every Secret in the cluster through the apiserver
directly. Choose `restricted` when that exposure is unacceptable. (Documented
trade-off — not a bug.)

Widen either preset with `rbac.extraRules`, appended verbatim. Keep verbs within
`get`/`list`/`watch` — the schema pins this list to read-only verbs.

## Safety gates and the env boundary

The chart's template gates catch dangerous **combinations** at render time. They
see **chart values only**: config delivered through `env`/`envFrom` (opaque
references the chart cannot read) bypasses them entirely. The backstop for those
is the **app's own startup checks** and `readout config validate` (see
[Validating before install](#validating-before-install)).

The gates:

- **No-auth exposure** — exposing through Ingress/Gateway while
  `config.auth.mode` is `none` is a template error. Set a real `auth.mode`
  (`oidc`/`headers`) or acknowledge with `unsafe.allowNoAuth: true`.
- **Multi-replica OIDC session secret** — `config.auth.mode: oidc` with
  `replicaCount > 1` and no chart-visible session secret is a template error
  (each replica would sign with its own ephemeral key and break OIDC login
  affinity). Wire one via `auth.sessionSecret.existingSecret`, an `env` entry
  named `READOUT_SESSION_SECRET`, or `config.sessionSecretFile`. An opaque
  `envFrom` source downgrades this to a NOTES reminder; or acknowledge with
  `unsafe.allowEphemeralSessionSecret: true`.
- **Metrics guards** — `config.metricsPort` set to a value different from
  `metrics.port` is an error; `config.metricsPort` set while `metrics.enabled` is
  false is an error; a `serviceMonitor` enabled without `metrics.enabled` does not
  render a useful object. Drive the metrics port through `metrics.port` only.

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

## Validating before install

Two complementary checks:

- **Chart values** — render the manifests and let the gates run:

  ```sh
  helm template readout oci://ghcr.io/kbelokon/charts/readout --version 0.8.0 -f my-values.yaml
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

Seven runnable values files in [`examples/`](examples):

| File | What it deploys |
| --- | --- |
| [`minimal.yaml`](examples/minimal.yaml) | Smallest honest install: single ClusterIP pod, wildcard preset, auth none, no exposure (reach via port-forward). |
| [`ingress-nginx.yaml`](examples/ingress-nginx.yaml) | readout behind a `networking.k8s.io/v1` Ingress on the nginx class. |
| [`gateway-api.yaml`](examples/gateway-api.yaml) | readout exposed by attaching an HTTPRoute to an existing Gateway API Gateway. |
| [`oidc-existing-secret.yaml`](examples/oidc-existing-secret.yaml) | OIDC login behind an Ingress, with session and client Secrets wired from pre-created Secrets. The recommended exposed setup. |
| [`metrics-servicemonitor.yaml`](examples/metrics-servicemonitor.yaml) | Separate metrics listener plus a Prometheus Operator ServiceMonitor. |
| [`argocd-cluster-secrets.yaml`](examples/argocd-cluster-secrets.yaml) | Argo CD cluster-Secret discovery with the least-privilege restricted preset. |
| [`extra-objects.yaml`](examples/extra-objects.yaml) | A platform CR (Traefik IngressRoute) shipped through the extraObjects escape hatch. |
