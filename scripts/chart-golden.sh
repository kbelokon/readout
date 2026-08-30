#!/usr/bin/env bash
# Golden renders of the chart's NetworkPolicy. It is a security object whose
# exact shape matters (WHICH peers get WHICH port), so an exit-code assert is
# not enough: each case under chart/ci/golden/ pairs <case>.values.yaml with
# the expected render <case>.yaml of templates/networkpolicy.yaml, and any
# drift fails with a diff. Runs identically on helm 3 (CI) and helm 4 (local).
#
#   scripts/chart-golden.sh [chart-dir]        # assert
#   UPDATE=1 scripts/chart-golden.sh [chart-dir]  # rewrite expected files
set -uo pipefail

CHART_DIR="${1:-chart}"
GOLDEN_DIR="$CHART_DIR/ci/golden"
TEMPLATE="templates/networkpolicy.yaml"
fail=0

# Expected files are stored NORMALIZED: comment lines (template prose, the
# `# Source:` header) and the two labels that change on every release bump are
# dropped, so neither a comment edit nor a version bump invalidates a golden.
normalize() {
  grep -v -E '^\s*(#|helm\.sh/chart:|app\.kubernetes\.io/version:)'
}

render() {
  helm template readout "$CHART_DIR" -s "$TEMPLATE" -f "$1" | normalize
}

for values in "$GOLDEN_DIR"/*.values.yaml; do
  case="$(basename "$values" .values.yaml)"
  expected="$GOLDEN_DIR/$case.yaml"
  if [ "${UPDATE:-0}" = "1" ]; then
    if render "$values" > "$expected.tmp"; then
      mv "$expected.tmp" "$expected"
      echo "updated: $case"
    else
      rm -f "$expected.tmp"
      echo "FAIL (render failed, expected file left untouched): $case"
      fail=1
    fi
    continue
  fi
  if [ ! -f "$expected" ]; then
    echo "FAIL (no expected file $expected; run with UPDATE=1 and review it): $case"
    fail=1
    continue
  fi
  if diff -u "$expected" <(render "$values"); then
    echo "ok (golden): $case"
  else
    echo "FAIL (render differs from $expected): $case"
    fail=1
  fi
done

exit "$fail"
