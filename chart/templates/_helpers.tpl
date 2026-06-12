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

{{/* Chart label value: name-version with non-DNS chars normalized. */}}
{{- define "readout.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Selector labels: the immutable release identity. */}}
{{- define "readout.selectorLabels" -}}
app.kubernetes.io/name: {{ include "readout.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Common labels applied to every resource. */}}
{{- define "readout.labels" -}}
helm.sh/chart: {{ include "readout.chart" . }}
{{ include "readout.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
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
