{{- define "vpa-gate.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "vpa-gate.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "vpa-gate.selectorLabels" -}}
app.kubernetes.io/name: {{ include "vpa-gate.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "vpa-gate.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "vpa-gate.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "vpa-gate.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "vpa-gate.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Namespace the controller watches ("" when watching all namespaces). */}}
{{- define "vpa-gate.watchNamespace" -}}
{{- if not .Values.watchAllNamespaces -}}
{{- default .Release.Namespace .Values.watchNamespace -}}
{{- end -}}
{{- end -}}
