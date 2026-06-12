#!/usr/bin/env bash
# Assert the chart's template safety gates and values.schema.json reject the
# inputs they must reject and accept the inputs they must accept. Each case
# checks the exit code of `helm template`, so it works identically on helm 3
# (CI) and helm 4 (local). A wrong exit code aborts the whole script non-zero.
set -uo pipefail

CHART_DIR="${1:-chart}"
fail=0

# expect_fail <description> -- the remaining args form a `helm template` call
# that MUST exit non-zero (a gate or schema rejection).
expect_fail() {
  local desc="$1"; shift
  if helm template readout "$CHART_DIR" "$@" >/dev/null 2>&1; then
    echo "FAIL (expected non-zero, got zero): $desc"
    fail=1
  else
    echo "ok (rejected): $desc"
  fi
}

# expect_pass <description> -- the call MUST exit zero.
expect_pass() {
  local desc="$1"; shift
  if helm template readout "$CHART_DIR" "$@" >/dev/null 2>&1; then
    echo "ok (accepted): $desc"
  else
    echo "FAIL (expected zero, got non-zero): $desc"
    fail=1
  fi
}

# Gate: multi-replica OIDC with no chart-visible session secret is rejected.
expect_fail "oidc multi-replica without session secret" \
  --set replicaCount=3 --set config.auth.mode=oidc
# ...and accepted once a session secret is wired through chart values.
expect_pass "oidc multi-replica with session secret" \
  --set replicaCount=3 --set config.auth.mode=oidc \
  --set auth.sessionSecret.existingSecret=s

# Gate: exposing a no-auth instance (ingress) is rejected by default...
expect_fail "ingress exposure while auth.mode=none" \
  --set ingress.enabled=true --set 'ingress.hosts[0].host=r.example.com'
# ...and unlocked only with the explicit unsafe acknowledgement.
expect_pass "ingress exposure with unsafe.allowNoAuth" \
  --set ingress.enabled=true --set 'ingress.hosts[0].host=r.example.com' \
  --set unsafe.allowNoAuth=true

# Schema: a non-integer replicaCount is a type error.
expect_fail "schema rejects non-integer replicaCount" \
  --set replicaCount=foo

# Schema: an rbac.extraRules verb outside get/list/watch is rejected.
expect_fail "schema rejects mutating extraRules verb" \
  --set 'rbac.extraRules[0].apiGroups[0]=x' \
  --set 'rbac.extraRules[0].resources[0]=y' \
  --set 'rbac.extraRules[0].verbs[0]=create'

exit "$fail"
