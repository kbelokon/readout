#!/usr/bin/env bash
# Assert the premise and the mitigation of the chart README's subchart caveat.
#
# This chart's safety model is "warn, never block": exposing a no-auth instance,
# or enabling a NetworkPolicy with no egress, RENDERS and warns through NOTES.
# Helm does not print a subchart's NOTES unless the installer asks for them, so
# an umbrella install silently drops every one of those warnings. The README
# tells operators to install with --render-subchart-notes; this file is what
# keeps that instruction from quietly becoming wrong.
#
# HELM 4 ONLY: helm 3's `install --dry-run=client` still reaches for a cluster,
# so the workflow calls this script from the helm 4 matrix leg alone rather than
# hiding a skip in here.
set -uo pipefail

CHART_DIR="${1:-chart}"
# Helm binary to drive: CI's chart job runs this on both majors. Locally,
# `mise exec helm@<version> -- ...` is enough; HELM takes a path to an
# executable installed outside mise.
HELM="${HELM:-helm}"

# A marker owned by THIS repository (chart/templates/NOTES.txt), not by Helm:
# rewording the warning is a deliberate act that must update this file too.
MARKER='WARNING: auth.mode is "none"'

fail=0
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/charts"
cp -R "$CHART_DIR" "$work/charts/readout"
cat > "$work/Chart.yaml" <<'EOF'
apiVersion: v2
name: wrapper
version: 0.1.0
dependencies:
  - name: readout
EOF
printf '{}\n' > "$work/values.yaml"

# Both outputs are captured from a SUCCESSFUL install and required to be
# non-empty first, so a broken install can never read as "the warning is absent".
if ! default_out="$("$HELM" install w "$work" --dry-run=client 2>&1)" || [ -z "$default_out" ]; then
  echo "FAIL (dry-run install failed): default output"
  fail=1
elif grep -qF "$MARKER" <<<"$default_out"; then
  echo "FAIL (subchart NOTES appeared by default -- the README caveat is stale): Helm default suppresses subchart safety NOTES (README premise)"
  fail=1
else
  echo "ok (suppressed): Helm default suppresses subchart safety NOTES (README premise)"
fi

if ! flagged_out="$("$HELM" install w "$work" --dry-run=client --render-subchart-notes 2>&1)" || [ -z "$flagged_out" ]; then
  echo "FAIL (dry-run install failed): --render-subchart-notes output"
  fail=1
elif grep -qF "$MARKER" <<<"$flagged_out"; then
  echo "ok (surfaced): --render-subchart-notes surfaces subchart safety warning (documented mitigation)"
else
  echo "FAIL (the documented mitigation no longer surfaces the warning): --render-subchart-notes surfaces subchart safety warning (documented mitigation)"
  fail=1
fi

exit "$fail"
