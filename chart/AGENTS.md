# AGENTS.md — deploying the readout chart

Guidance for an agent installing the **readout** Helm chart for a user. readout
is a strictly read-only Kubernetes web viewer. It serves on its own host
(subdomain or root domain) — no subpath. Follow this flow top to bottom; each
step is concrete commands, not theory.

Chart source (OCI): `oci://ghcr.io/kbelokon/charts/readout` (version `0.10.1`).

## 1. Detect cluster routing options

Find out what the target cluster can actually do before choosing an exposure:

```sh
kubectl config current-context          # confirm you target the right cluster
kubectl api-resources | grep -Ei 'ingress|gateway|httproute'
kubectl get ingressclass
kubectl get gatewayclass
kubectl get gateways -A
```

## 2. Choose exposure (decision table)

| Cluster state | Choose |
| --- | --- |
| An `ingressclass` exists, no Gateway API | **Ingress** (`ingress.enabled`) |
| A `gatewayclass` and at least one `Gateway` exist | **Gateway API** (`gateway.enabled` + `parentRefs`) |
| Routing is a controller CR (Traefik IngressRoute, etc.) | **extraObjects** escape hatch |
| No routing yet / dev / probe-only | **No exposure** — `kubectl port-forward` |

readout serves on its own host. There is no subpath option; `publicUrl` is
origin-only (scheme + host).

## 3. Wire auth and secrets

The chart **never creates Secrets**. Create them first, then reference by name.

Recommended (typed OIDC wiring):

```sh
kubectl create secret generic readout-session \
  --from-literal=session-secret="$(openssl rand -hex 32)"
kubectl create secret generic readout-oidc \
  --from-literal=client-id=YOUR_OIDC_CLIENT_ID \
  --from-literal=client-secret=YOUR_OIDC_CLIENT_SECRET
```

```yaml
# values: typed wiring -> READOUT_SESSION_SECRET / READOUT_OIDC_CLIENT_ID / _SECRET
auth:
  sessionSecret: {existingSecret: readout-session, key: session-secret}
  oidc: {existingSecret: readout-oidc, clientIdKey: client-id, clientSecretKey: client-secret}
config:
  auth:
    mode: oidc
    oidc: {issuerUrl: https://idp.example.com/}
  publicUrl: https://readout.example.com
```

Auth posture rules:

- Exposing (Ingress/Gateway/LoadBalancer/NodePort) with `config.auth.mode: none`
  is **not blocked** — the chart installs and prints a loud NOTES warning. It
  publishes an unauthenticated, cluster-wide read viewer, so set a real
  `auth.mode` (`oidc`/`headers`) before trusting the exposure.
- `auth.mode: oidc` with `replicaCount > 1` needs a chart-visible session secret
  shared by every replica (the typed `auth.sessionSecret` above satisfies it).
  Without one the chart still installs but warns in NOTES: each pod signs sessions
  with its own ephemeral key, so OIDC login breaks under load balancing unless you
  have sticky sessions.
- For anything the typed surface misses, use `env` (literal `valueFrom`) or
  `envFrom` (whole-Secret reference).

## 4. Validate

Two layers — run both before installing.

Chart values (renders manifests, runs the safety gates):

```sh
helm template readout oci://ghcr.io/kbelokon/charts/readout \
  --version 0.10.1 -f values.yaml
```

Raw app config (faithful to startup):

```sh
readout config validate --config readout.yaml   # exit 0 + "config OK"; exit 1 on error; exit 2 usage
```

**Caveat:** `config validate` honors `READOUT_*` env vars in the calling shell
(env overrides the file). Validate in the same environment the pod will run in,
or unset stray `READOUT_*` vars first.

## 5. Install

Dry-run first, then apply:

```sh
helm upgrade --install readout oci://ghcr.io/kbelokon/charts/readout \
  --version 0.10.1 -f values.yaml --dry-run
helm upgrade --install readout oci://ghcr.io/kbelokon/charts/readout \
  --version 0.10.1 -f values.yaml
```

**Upgrading a user from chart ≤ 0.6:** identity (selectors/names) changed, so
`helm upgrade` fails. readout is stateless — uninstall then install, no data loss:

```sh
helm uninstall readout
helm install readout oci://ghcr.io/kbelokon/charts/readout --version 0.10.1 -f values.yaml
```

## 6. Verify

```sh
kubectl rollout status deploy/readout
kubectl get pods -l app.kubernetes.io/name=readout
kubectl port-forward svc/readout 8080:80 &
curl -sf http://localhost:8080/readyz && echo " ready"
```

When using Gateway API, also confirm the route was accepted:

```sh
kubectl describe httproute readout    # check Parents/Conditions: Accepted=True, ResolvedRefs=True
```

## 7. Troubleshoot (gate failure → the value that fixes it)

The chart `fail`s the render ONLY for combinations the Kubernetes API would
reject anyway (failing early with a clear message). Security/operational postures
— no-auth exposure, multi-replica OIDC without a shared session secret — are
**never render-blocked**: they install and warn in NOTES (see §3). Map a render
failure to its fix:

| Failure message contains | Fix |
| --- | --- |
| `commonLabels must not set` / `podLabels must not set` (an immutable identity label) | Use any label key other than `app.kubernetes.io/name`/`app.kubernetes.io/instance`; those build the immutable Deployment/Service selectors. |
| `podDisruptionBudget: minAvailable (...) and maxUnavailable (...) are mutually exclusive` | Set exactly one of `podDisruptionBudget.minAvailable`/`maxUnavailable` — Kubernetes rejects both. |
| `config.metricsPort (...) conflicts with metrics.port (...)` | Unset `config.metricsPort` and drive the port through `metrics.port`, or make them equal. |
| `config.metricsPort (...) is set but metrics.enabled is false` | Set `metrics.enabled: true` (and `metrics.port`) instead of setting `config.metricsPort` directly. |
| HTTPRoute invalid / route never attaches | `gateway.parentRefs` is required when `gateway.enabled` — name the Gateway(s) to attach to. |
| `serviceMonitor` renders nothing useful | Enable `metrics.enabled: true`; the ServiceMonitor needs the metrics Service. |

Remember the **env boundary**: the gates see chart values only. Config delivered
through `env`/`envFrom` is opaque to them — if a misconfiguration slips past the
gates, `readout config validate` and the app's startup checks are the backstop.
