#!/usr/bin/env bash
# Assert the chart's template safety gates and values.schema.json reject the
# inputs they must reject and accept the inputs they must accept. Each case
# checks the exit code of `helm template` -- or, for expect_grep /
# expect_no_grep, the presence or absence of a line of its rendered output -- so
# CI runs it on helm 3 and helm 4 alike (the chart job matrix). A wrong exit code, a
# missing rendered line, or a line that must not render, exits non-zero.
set -uo pipefail

# Helm binary to drive: CI's chart job runs this on both majors. Locally,
# `mise exec helm@<version> -- ...` is enough; HELM takes a path to an
# executable installed outside mise.
HELM="${HELM:-helm}"

CHART_DIR="${1:-chart}"
fail=0

# expect_fail <description> -- the remaining args form a `helm template` call
# that MUST exit non-zero (a gate or schema rejection).
expect_fail() {
  local desc="$1"; shift
  if "$HELM" template readout "$CHART_DIR" "$@" >/dev/null 2>&1; then
    echo "FAIL (expected non-zero, got zero): $desc"
    fail=1
  else
    echo "ok (rejected): $desc"
  fi
}

# expect_pass <description> -- the call MUST exit zero.
expect_pass() {
  local desc="$1"; shift
  if "$HELM" template readout "$CHART_DIR" "$@" >/dev/null 2>&1; then
    echo "ok (accepted): $desc"
  else
    echo "FAIL (expected zero, got non-zero): $desc"
    fail=1
  fi
}

# expect_grep <description> <extended-regex> -- the remaining args form a
# `helm template` call whose rendered output MUST contain a line matching the
# regex. Use it to pin a rendered VALUE, not just an exit code.
expect_grep() {
  local desc="$1" pattern="$2"; shift 2
  if "$HELM" template readout "$CHART_DIR" "$@" 2>/dev/null | grep -q -E "$pattern"; then
    echo "ok (rendered): $desc"
  else
    echo "FAIL (pattern not rendered: $pattern): $desc"
    fail=1
  fi
}

# expect_no_grep <description> <extended-regex> -- the render MUST succeed and
# MUST NOT contain a line matching the regex. The render is captured first so
# a failing helm (empty output) is a FAIL, never a false "absent".
expect_no_grep() {
  local desc="$1" pattern="$2"; shift 2
  local out
  if ! out="$("$HELM" template readout "$CHART_DIR" "$@" 2>/dev/null)" || [ -z "$out" ]; then
    echo "FAIL (render failed or empty): $desc"
    fail=1
  elif grep -q -E "$pattern" <<<"$out"; then
    echo "FAIL (pattern rendered but must be absent: $pattern): $desc"
    fail=1
  else
    echo "ok (absent): $desc"
  fi
}

# Multi-replica OIDC with no chart-visible session secret is NEVER render-blocked:
# it renders and warns via NOTES (each replica would otherwise sign with its own
# ephemeral key; the operator is warned, not stopped).
expect_pass "oidc multi-replica without session secret renders (warns, never blocks)" \
  --set replicaCount=3 --set config.auth.mode=oidc
# ...and also renders once a session secret is wired through chart values.
expect_pass "oidc multi-replica with session secret renders" \
  --set replicaCount=3 --set config.auth.mode=oidc \
  --set auth.sessionSecret.existingSecret=s

# No-auth exposure is NEVER render-blocked: exposing a no-auth instance through
# ingress renders successfully (the operator is warned via NOTES, not stopped).
expect_pass "ingress exposure while auth.mode=none renders (warns, never blocks)" \
  --set ingress.enabled=true --set 'ingress.hosts[0].host=r.example.com'
# A no-auth LoadBalancer Service is likewise rendered, not blocked.
expect_pass "LoadBalancer exposure while auth.mode=none renders (warns, never blocks)" \
  --set service.type=LoadBalancer

# Schema: a non-integer replicaCount is a type error.
expect_fail "schema rejects non-integer replicaCount" \
  --set replicaCount=foo

# Schema: an rbac.extraRules verb outside get/list/watch is rejected.
expect_fail "schema rejects mutating extraRules verb" \
  --set 'rbac.extraRules[0].apiGroups[0]=x' \
  --set 'rbac.extraRules[0].resources[0]=y' \
  --set 'rbac.extraRules[0].verbs[0]=create'

# Gate: selector-identity labels cannot be overridden via label knobs.
expect_fail "gate rejects commonLabels overriding selector identity" \
  --set 'commonLabels.app\.kubernetes\.io/instance=evil'
# Gate: a PDB with both budget fields set is rejected at render time.
expect_fail "gate rejects PDB with both minAvailable and maxUnavailable" \
  --set podDisruptionBudget.enabled=true \
  --set podDisruptionBudget.minAvailable=1 \
  --set podDisruptionBudget.maxUnavailable=1
# A name-only env husk is not a real session-secret source, but it is NOT
# render-blocked: the chart renders and NOTES warns (the husk renders an empty
# env var and the app would fall back to an ephemeral per-pod key).
expect_pass "oidc multi-replica with name-only env husk renders (warns, never blocks)" \
  --set replicaCount=3 --set config.auth.mode=oidc \
  --set 'env[0].name=READOUT_SESSION_SECRET'

# Schema conditional (if/then) branches -- the constructs most likely to
# diverge between the helm 3 and helm 4 schema engines; the chart job matrix
# runs this file on both.
expect_fail "schema rejects ingress.enabled with zero hosts" \
  --set ingress.enabled=true
expect_fail "schema rejects existingSecret with empty key" \
  --set auth.oidc.existingSecret=s --set auth.oidc.clientIdKey=""

# Gate: metrics.port on the app port. Whichever of the two listeners binds
# second fails: the pod crash-loops, or /metrics silently vanishes (404 on the
# main port). Rejected at render time instead of surfacing at runtime.
expect_fail "gate rejects metrics.port equal to config.port" \
  --set metrics.enabled=true --set metrics.port=8080

# Gate: metrics peers named while there is no metrics port. Without the gate
# the metricsFrom rule silently does not render and the operator believes the
# scraper is admitted.
expect_fail "gate rejects networkPolicy.ingress.metricsFrom without metrics.enabled" \
  --set networkPolicy.enabled=true \
  --set 'networkPolicy.ingress.metricsFrom[0].namespaceSelector.matchLabels.kubernetes\.io/metadata\.name=monitoring'
expect_pass "networkPolicy.ingress.metricsFrom with metrics.enabled renders" \
  --set networkPolicy.enabled=true --set metrics.enabled=true \
  --set 'networkPolicy.ingress.metricsFrom[0].namespaceSelector.matchLabels.kubernetes\.io/metadata\.name=monitoring'

# Rendered value: the helm-test pod must carry the component label the
# NetworkPolicy's helm-test rule selects...
expect_grep "helm-test pod carries app.kubernetes.io/component: test-connection" \
  '^\s*app\.kubernetes\.io/component: test-connection$' \
  --set testFramework.enabled=true -s templates/tests/test-connection.yaml
# ...and must NOT carry app.kubernetes.io/name: every chart selector matches
# name AND instance, so omitting name keeps the test pod out of the Deployment,
# Service, PDB and NetworkPolicy podSelectors (it is a client, not a replica).
expect_no_grep "helm-test pod carries no app.kubernetes.io/name" \
  '^\s*app\.kubernetes\.io/name:' \
  --set testFramework.enabled=true -s templates/tests/test-connection.yaml

# Rendered value: the app binds LOOPBACK when listenAddress is empty under
# auth.mode none (its safe default for a bare binary). Inside a pod that means
# kubelet probes and the Service cannot reach it, so the chart must render an
# explicit all-interfaces bind by default.
expect_grep "default config binds all interfaces (loopback is unreachable in a pod)" \
  '^\s*listenAddress: "?0\.0\.0\.0"?$' -s templates/configmap.yaml

# Schema: the two top-level keys a PARENT chart owns are accepted. Helm copies
# its `global` table into every subchart's values (even when the parent sets
# none), and `condition: <name>.enabled` puts `enabled` in the child namespace.
# A chart whose root schema rejects them cannot be used as a dependency at all.
# The real subchart path is exercised by scripts/chart-umbrella.sh; these two
# only pin the schema itself.
expect_pass "schema accepts a parent-injected global block" \
  --set-json 'global={"imageRegistry":"mirror.example.com"}'
expect_pass "schema accepts a parent-injected enabled flag" \
  --set enabled=true
# ...while every OTHER unknown top-level key stays rejected, in an umbrella's
# child namespace exactly as standalone.
expect_fail "schema still rejects an unknown top-level key" \
  --set replicaCounttypo=3

# Pull secrets reach BOTH pods that pull an image -- the helm-test pod pulls a
# different image from a different registry. They are pinned on the PodSpecs
# rather than on a ServiceAccount, which would tie them to an account's
# identity and lifecycle and differ per managed/existing/default account.
expect_grep "imagePullSecrets reach the readout Deployment" \
  '^\s*- name: regcred$' \
  --set 'imagePullSecrets[0].name=regcred' -s templates/deployment.yaml
expect_grep "imagePullSecrets reach the helm-test pod" \
  '^\s*- name: regcred$' \
  --set 'imagePullSecrets[0].name=regcred' --set testFramework.enabled=true \
  -s templates/tests/test-connection.yaml
# Only the Kubernetes-native shape is accepted: a list of {name: <secret>}.
# A bare string, a missing name, and an empty name are all rejected rather than
# rendering a pull secret Kubernetes will silently ignore.
expect_fail "schema rejects a string-form imagePullSecrets entry" \
  --set-json 'imagePullSecrets=["regcred"]'
expect_fail "schema rejects an imagePullSecrets entry without a name" \
  --set-json 'imagePullSecrets=[{"secret":"regcred"}]'
expect_fail "schema rejects an empty imagePullSecrets name" \
  --set-json 'imagePullSecrets=[{"name":""}]'

exit "$fail"
