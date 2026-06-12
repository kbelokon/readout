{{/*
Cross-field safety gates. Included unconditionally from deployment.yaml (the
Deployment always renders, so this always evaluates). Schema validation handles
shapes and enums; this handles the dangerous COMBINATIONS that a per-field
schema cannot express. Every gate sees chart values only -- config delivered
through env / envFrom is opaque to the chart and is caught instead by the app's
startup checks and `readout config validate`.
*/}}
{{- define "readout.validate" -}}
{{- include "readout.validate.sessionSecret" . -}}
{{- include "readout.validate.noAuth" . -}}
{{- include "readout.validate.selectorLabels" . -}}
{{- include "readout.validate.pdb" . -}}
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
Gate (b): multi-replica OIDC needs a STABLE session secret shared across pods,
or each replica signs sessions with its own ephemeral key and OIDC login breaks
under load balancing. Fail when mode is oidc and replicaCount > 1 and the chart
sees NO session-secret wiring, unless an opaque envFrom source is present (then
we assume the secret may come from there; NOTES reminds the operator) or the
operator acknowledges via unsafe.allowEphemeralSessionSecret.
*/}}
{{- define "readout.validate.sessionSecret" -}}
{{- if and (eq (.Values.config.auth).mode "oidc") (gt (int .Values.replicaCount) 1) (not .Values.unsafe.allowEphemeralSessionSecret) -}}
  {{- $wired := false -}}
  {{- if .Values.auth.sessionSecret.existingSecret -}}
    {{- $wired = true -}}
  {{- end -}}
  {{- if .Values.config.sessionSecretFile -}}
    {{- $wired = true -}}
  {{- end -}}
  {{- range .Values.env -}}
    {{- /* The entry must actually carry something (value or valueFrom) -- a
       name-only husk renders an empty env var and the app falls back to an
       ephemeral per-pod key, exactly what this gate exists to catch. */ -}}
    {{- if and (eq .name "READOUT_SESSION_SECRET") (or .value .valueFrom) -}}
      {{- $wired = true -}}
    {{- end -}}
  {{- end -}}
  {{- if and (not $wired) (not .Values.envFrom) -}}
    {{- fail "config.auth.mode=oidc with replicaCount>1 needs a stable session secret shared by every replica; without one each pod signs sessions with its own ephemeral key and OIDC login breaks under load balancing. Wire one through chart values: auth.sessionSecret.existingSecret (+key), or an env[] entry named READOUT_SESSION_SECRET, or config.sessionSecretFile, or supply it via envFrom. To run anyway with an ephemeral per-pod secret, set unsafe.allowEphemeralSessionSecret=true." -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/*
Gate (c): exposing readout (ingress or gateway) while auth.mode is none/unset
puts an unauthenticated cluster viewer on the network. Fail unless the operator
acknowledges via unsafe.allowNoAuth.
*/}}
{{- define "readout.validate.noAuth" -}}
{{- if and (or .Values.ingress.enabled .Values.gateway.enabled) (not .Values.unsafe.allowNoAuth) -}}
  {{- $mode := (.Values.config.auth).mode | default "none" -}}
  {{- if eq $mode "none" -}}
    {{- fail "exposing readout through ingress/gateway while config.auth.mode is none publishes an unauthenticated, cluster-wide read viewer; anyone who reaches the address can browse your cluster. Set config.auth.mode to a real mode (oidc/headers) before exposing it, or set unsafe.allowNoAuth=true to expose it without authentication on purpose." -}}
  {{- end -}}
{{- end -}}
{{- end -}}
