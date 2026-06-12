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
    {{- if eq .name "READOUT_SESSION_SECRET" -}}
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
