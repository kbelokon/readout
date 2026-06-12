{{/* Chart name, overridable via nameOverride. */}}
{{- define "readout.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name. fullnameOverride wins outright; otherwise if the
release name already contains the chart name we use the release name as-is,
else we prefix it with the release name. Truncated to 63 chars for DNS names.
*/}}
{{- define "readout.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Fullname with a suffix appended, keeping the WHOLE name within the 63-char
DNS-label limit. A plain `fullname-suffix` concatenation can reach 71 chars
for a long release name, which renders fine and is rejected only by the
cluster API. Call with (dict "context" . "suffix" "metrics").
*/}}
{{- define "readout.fullname.suffixed" -}}
{{- $max := int (sub 62 (len .suffix)) -}}
{{- printf "%s-%s" (include "readout.fullname" .context | trunc $max | trimSuffix "-") .suffix -}}
{{- end -}}

{{/* Chart label value: name-version with non-DNS chars normalized. */}}
{{- define "readout.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Selector labels: the immutable release identity. */}}
{{- define "readout.selectorLabels" -}}
app.kubernetes.io/name: {{ include "readout.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Common labels applied to every resource. commonLabels are user-supplied
and merged last so they can override the standard label set. */}}
{{- define "readout.labels" -}}
helm.sh/chart: {{ include "readout.chart" . }}
{{ include "readout.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
Common annotations applied to every resource. Emits only the merged key/value
lines (no annotations: header) so call sites can guard an empty result and keep
the default render free of stray annotation blocks. Call as:
  {{- $ann := include "readout.annotations" (dict "context" . "local" .Values.x.annotations) }}
where "local" is an optional per-resource annotation map merged over the common
set. Returns an empty string when nothing is set.
*/}}
{{- define "readout.annotations" -}}
{{- $context := .context -}}
{{- $merged := merge (deepCopy (default (dict) .local)) (default (dict) $context.Values.commonAnnotations) -}}
{{- if $merged -}}
{{- toYaml $merged -}}
{{- end -}}
{{- end -}}

{{/*
Effective app config as YAML. metrics.enabled renders config.metricsPort from
metrics.port; a hand-set config.metricsPort must agree (equal value) or the
chart fails, and setting it while metrics is disabled fails too. This is the
single render path -- the ConfigMap serializes this and the Deployment hashes
it for checksum/config, so changing metrics.port rolls pods.
*/}}
{{- define "readout.config" -}}
{{- $cfg := deepCopy .Values.config -}}
{{- $given := .Values.config.metricsPort | default 0 -}}
{{- if .Values.metrics.enabled -}}
  {{- if and (ne (int $given) 0) (ne (int $given) (int .Values.metrics.port)) -}}
    {{- fail (printf "config.metricsPort (%v) conflicts with metrics.port (%v): unset config.metricsPort or make them equal" $given .Values.metrics.port) -}}
  {{- end -}}
  {{- $_ := set $cfg "metricsPort" .Values.metrics.port -}}
{{- else if ne (int $given) 0 -}}
  {{- fail (printf "config.metricsPort (%v) is set but metrics.enabled is false: the app would move /metrics off the main port with no metrics Service. Set metrics.enabled=true (and metrics.port) instead" $given) -}}
{{- end -}}
{{- toYaml $cfg -}}
{{- end -}}

{{/* ServiceAccount name: fullname when created, else the provided name or "default". */}}
{{- define "readout.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- include "readout.fullname" . -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Image reference. A digest wins outright (tag ignored); otherwise the tag,
defaulting to the chart appVersion when unset.
*/}}
{{- define "readout.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end -}}
