#!/usr/bin/env bash
# Render the chart (default values + every examples/*.yaml) and validate each
# manifest stream with kubeconform in strict mode. Core Kubernetes types use
# the default upstream schema location; the three custom-resource kinds the
# chart can emit (Gateway API HTTPRoute, Prometheus ServiceMonitor, Traefik
# IngressRoute) resolve against the schemas vendored under chart/ci/schemas/.
# We never pass -ignore-missing-schemas: an unknown kind must fail, not skip.
set -euo pipefail

CHART_DIR="${1:-chart}"
SCHEMA_LOCATION="${CHART_DIR}/ci/schemas/{{ .Group }}/{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json"

validate() {
  kubeconform -strict \
    -schema-location default \
    -schema-location "$SCHEMA_LOCATION"
}

echo "==> default render"
helm template readout "$CHART_DIR" | validate

for f in "$CHART_DIR"/examples/*.yaml; do
  echo "==> example: $f"
  helm template readout "$CHART_DIR" -f "$f" | validate
done

echo "OK"
