#!/usr/bin/env bash
# Assert that every place naming the application image agrees with appVersion:
#
#   appVersion  ==  artifacthub.io/images[name=readout] tag  ==  default render
#
# Chart.version is deliberately NOT part of that equality. A chart-only patch
# moves the chart while the application stands still, and the Artifact Hub
# annotation is the one place carrying a hardcoded tag that nothing else pins --
# so it is exactly what drifts silently once the two versions separate.
#
# Run from CI on every PR (early, fixable with a commit) and again from the
# release workflow (a tag can be pushed at a commit CI never passed).
set -uo pipefail

CHART_DIR="${1:-chart}"
# Helm binary to drive: CI's chart job runs this on both majors. Locally,
# `mise exec helm@<version> -- ...` is enough; HELM takes a path to an
# executable installed outside mise.
HELM="${HELM:-helm}"
YQ="${YQ:-yq}"

# Mike Farah's yq v4, not the Python wrapper of the same name and not a future
# major: the expressions below are that dialect, and another one would parse
# them differently -- silently, since a wrong parse still prints something.
yq_version="$("$YQ" --version 2>&1)" || yq_version="none"
case "$yq_version" in
  *github.com/mikefarah/yq*version\ v4.*) ;;
  *) echo "FAIL: need Mike Farah's yq v4 on PATH (got: $yq_version)"; exit 1 ;;
esac

fail=0
die() { echo "FAIL: $1"; fail=1; }

version="$("$YQ" -r '.version' "$CHART_DIR/Chart.yaml")"
appversion="$("$YQ" -r '.appVersion' "$CHART_DIR/Chart.yaml")"
[ -n "$version" ] && [ "$version" != "null" ] || die "Chart.yaml has no version"
[ -n "$appversion" ] && [ "$appversion" != "null" ] || die "Chart.yaml has no appVersion"

# Exactly one readout entry in the annotation: a second one, or none, means the
# equality below would be silently checking the wrong thing.
entries="$("$YQ" -r '.annotations."artifacthub.io/images"' "$CHART_DIR/Chart.yaml" \
  | "$YQ" -r '[.[] | select(.name == "readout")] | length')"
[ "$entries" = "1" ] || die "artifacthub.io/images must hold exactly one entry named readout (found $entries)"

annotated="$("$YQ" -r '.annotations."artifacthub.io/images"' "$CHART_DIR/Chart.yaml" \
  | "$YQ" -r '.[] | select(.name == "readout") | .image')"

# Select the container by NAME: extraContainers makes the ordering a user's
# choice, so an index would pin something this chart does not own.
rendered="$("$HELM" template readout "$CHART_DIR" -s templates/deployment.yaml \
  | "$YQ" -r '.spec.template.spec.containers[] | select(.name == "readout") | .image')"
[ -n "$rendered" ] && [ "$rendered" != "null" ] || die "could not read the rendered readout image"

expected="ghcr.io/kbelokon/readout:${appversion}"
[ "$annotated" = "$expected" ] || die "artifacthub.io/images is $annotated, expected $expected"
[ "$rendered" = "$expected" ] || die "the default render is $rendered, expected $expected"

if [ "$fail" = 0 ]; then
  echo "ok: appVersion $appversion == annotation == default render (chart version $version, free to differ)"
fi
exit "$fail"
