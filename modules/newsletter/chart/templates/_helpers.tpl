{{/* Shared by the starter module charts (support, newsletter, analytics, wiki, crm, status): keep identical. */}}
{{- define "mod.selector" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "mod.labels" -}}
{{ include "mod.selector" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: companyinacan
can.dev/module: {{ .Chart.Name }}
{{- end -}}

{{/* repository:tag, plus @digest once the digest is a real one (placeholders are for development). */}}
{{- define "mod.image" -}}
{{- $d := .digest | default "" -}}
{{- if and $d (not (contains "REPLACE_WITH_" $d)) -}}
{{ .repository }}:{{ .tag }}@{{ $d }}
{{- else -}}
{{ .repository }}:{{ .tag }}
{{- end -}}
{{- end -}}

{{- define "mod.scheme" -}}
{{ ternary "https" "http" .Values.ingress.tls }}
{{- end -}}
