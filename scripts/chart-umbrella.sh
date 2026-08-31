#!/usr/bin/env bash
# Render the chart AS A DEPENDENCY of a throwaway parent chart. Helm coalesces
# the parent's `global` table into every subchart's values unconditionally --
# even when the parent sets none -- and `condition: <name>.enabled` lands
# `enabled` in the child's own namespace. A root schema with
# additionalProperties:false must therefore declare both, or the chart cannot be
# a dependency at all: the render aborts before a single object is produced.
#
# The parent is built UNPACKED (the chart is copied into charts/), so no
# `helm dependency build`, no registry and no network are involved, and the
# dependency carries no version constraint to keep in step with Chart.yaml.
set -uo pipefail

CHART_DIR="${1:-chart}"

# Helm binary to drive: CI's chart job runs this on both majors. Locally,
# `mise exec helm@<version> -- ...` is enough; HELM takes a path to an
# executable installed outside mise.
HELM="${HELM:-helm}"

fail=0
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# expect_render <description> -- the parent in $work MUST render, and its output
# MUST contain the subchart's Deployment. A zero exit with an empty render (the
# `condition` never matched, say) is a FAIL, not a pass.
expect_render() {
  local desc="$1" out
  if ! out="$("$HELM" template w "$work" 2>&1)"; then
    echo "FAIL (render failed): $desc"
    printf '%s\n' "$out" | sed 's/^/    /' | head -5
    fail=1
  elif ! grep -q '^kind: Deployment$' <<<"$out"; then
    echo "FAIL (rendered nothing from the subchart): $desc"
    fail=1
  else
    echo "ok (rendered as a subchart): $desc"
  fi
}

mkdir -p "$work/charts"
cp -R "$CHART_DIR" "$work/charts/readout"

# Case 1: a plain dependency. The parent declares no `global` of its own; Helm
# injects the key regardless, which is the whole point of the case.
cat > "$work/Chart.yaml" <<'EOF'
apiVersion: v2
name: wrapper
version: 0.1.0
dependencies:
  - name: readout
EOF
cat > "$work/values.yaml" <<'EOF'
global:
  sentinel: from-the-parent
EOF
expect_render "plain dependency with a parent global block"

# Case 2: the dependency is gated by a Helm `condition`, whose value path sits
# inside the subchart's OWN namespace -- so `enabled` reaches the child too.
cat > "$work/Chart.yaml" <<'EOF'
apiVersion: v2
name: wrapper
version: 0.1.0
dependencies:
  - name: readout
    condition: readout.enabled
EOF
cat > "$work/values.yaml" <<'EOF'
global:
  sentinel: from-the-parent
readout:
  enabled: true
EOF
expect_render "dependency gated by condition: readout.enabled"

# The strict root schema still does its job through the parent: a typo in the
# umbrella's own values must be rejected, exactly as it is standalone.
if "$HELM" template w "$work" --set readout.replicaCounttypo=3 >/dev/null 2>&1; then
  echo "FAIL (expected non-zero, got zero): parent-side typo in child values is rejected"
  fail=1
else
  echo "ok (rejected): parent-side typo in child values"
fi

exit "$fail"
