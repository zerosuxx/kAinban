{{/*
Expand the name of the chart.
*/}}
{{- define "kainban.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "kainban.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart name and version as used by the chart label.
*/}}
{{- define "kainban.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "kainban.labels" -}}
helm.sh/chart: {{ include "kainban.chart" . }}
{{ include "kainban.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "kainban.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kainban.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Name of the ServiceAccount to use.
*/}}
{{- define "kainban.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kainban.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Container image reference.
*/}}
{{- define "kainban.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}

{{/*
Ollama: its own selector, so the orchestrator Deployment never selects it.
*/}}
{{- define "kainban.ollama.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kainban.name" . }}-ollama
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "kainban.ollama.labels" -}}
helm.sh/chart: {{ include "kainban.chart" . }}
{{ include "kainban.ollama.selectorLabels" . }}
app.kubernetes.io/component: ollama
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
In-cluster Ollama URL ("" when disabled).
*/}}
{{- define "kainban.ollama.url" -}}
{{- if .Values.ollama.enabled }}
{{- printf "http://%s-ollama.%s.svc:11434" (include "kainban.fullname" .) .Release.Namespace }}
{{- end }}
{{- end }}
