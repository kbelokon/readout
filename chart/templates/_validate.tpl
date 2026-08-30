{{/*
Cross-field safety gates. Included unconditionally from deployment.yaml (the
Deployment always renders, so this always evaluates). Schema validation handles
shapes and enums; this handles the dangerous COMBINATIONS that a per-field
schema cannot express. Every gate sees chart values only -- config delivered
through env / envFrom is opaque to the chart and is caught instead by the app's
startup checks and `readout config validate`.
*/}}
{{- define "readout.validate" -}}
{{- include "readout.validate.selectorLabels" . -}}
{{- include "readout.validate.pdb" . -}}
{{- include "readout.validate.ports" . -}}
{{- include "readout.validate.networkPolicy" . -}}
{{- end -}}

{{/*
app.kubernetes.io/name and app.kubernetes.io/instance ARE the release identity:
the Deployment selector and Service selector are built from them and selectors
are immutable. A commonLabels/podLabels override produces duplicate label keys
and a selector/template mismatch that the cluster API rejects with a generic
message; fail at render time naming the offending key instead.
*/}}
{{- define "readout.validate.selectorLabels" -}}
{{- range $key := list "app.kubernetes.io/name" "app.kubernetes.io/instance" -}}
  {{- if hasKey ($.Values.commonLabels | default dict) $key -}}
    {{- fail (printf "commonLabels must not set %q: it is the immutable release identity the Deployment/Service selectors are built from. Use any other label key." $key) -}}
  {{- end -}}
  {{- if hasKey ($.Values.podLabels | default dict) $key -}}
    {{- fail (printf "podLabels must not set %q: it is the immutable release identity the Deployment/Service selectors are built from. Use any other label key." $key) -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/*
A PodDisruptionBudget may carry minAvailable OR maxUnavailable, never both --
the cluster API rejects both set, but only at install time; renders and schema
validation pass. Fail at render time instead.
*/}}
{{- define "readout.validate.pdb" -}}
{{- if .Values.podDisruptionBudget.enabled -}}
  {{- $min := .Values.podDisruptionBudget.minAvailable -}}
  {{- $max := .Values.podDisruptionBudget.maxUnavailable -}}
  {{- $minSet := not (or (kindIs "invalid" $min) (eq (toString $min) "")) -}}
  {{- $maxSet := not (or (kindIs "invalid" $max) (eq (toString $max) "")) -}}
  {{- if and $minSet $maxSet -}}
    {{- fail (printf "podDisruptionBudget: minAvailable (%v) and maxUnavailable (%v) are mutually exclusive; set exactly one." $min $max) -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/*
metrics.port equal to config.port puts both listeners on one port. The app
starts them in two goroutines with no ordering, so whichever binds second
fails: if it is the app listener the process exits and the pod crash-loops;
if it is the metrics listener the app logs and keeps serving while /metrics
on the main port answers 404 (it moves off the main mux whenever metricsPort
is set) -- metrics silently vanish. Fail at render time naming both keys.
*/}}
{{- define "readout.validate.ports" -}}
{{- if .Values.metrics.enabled -}}
  {{- $app := int (.Values.config.port | default 8080) -}}
  {{- if eq (int .Values.metrics.port) $app -}}
    {{- fail (printf "metrics.port (%v) equals config.port (%v): both listeners would try to bind one port, so one of them fails -- the pod crash-loops or /metrics silently disappears. Pick a different metrics.port." .Values.metrics.port $app) -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/*
networkPolicy.ingress.metricsFrom names peers for the metrics port. With
metrics.enabled false there is no metrics port, so the rule would silently not
render and the operator would believe the scraper is admitted. Fail instead.
*/}}
{{- define "readout.validate.networkPolicy" -}}
{{- if and .Values.networkPolicy.enabled .Values.networkPolicy.ingress.metricsFrom (not .Values.metrics.enabled) -}}
  {{- fail "networkPolicy.ingress.metricsFrom is set but metrics.enabled is false: there is no metrics port to open. Set metrics.enabled=true, or move those peers to networkPolicy.ingress.from if they really need the app port." -}}
{{- end -}}
{{- end -}}
