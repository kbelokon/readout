{{- define "readout.name" -}}
readout
{{- end -}}

{{- define "readout.labels" -}}
application: {{ include "readout.name" . }}
app.kubernetes.io/name: {{ include "readout.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
