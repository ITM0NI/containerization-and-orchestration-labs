{{- define "lab2-alerts.selectorLabels" -}}
app.kubernetes.io/name: lab2-alert-receiver
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "lab2-alerts.labels" -}}
{{ include "lab2-alerts.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | quote }}
{{- end }}
