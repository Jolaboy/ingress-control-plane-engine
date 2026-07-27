{{/*
Expand the name of the chart.
*/}}
{{- define "icpe.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "icpe.fullname" -}}
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
Chart label
*/}}
{{- define "icpe.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to all resources
*/}}
{{- define "icpe.labels" -}}
helm.sh/chart: {{ include "icpe.chart" . }}
{{ include "icpe.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "icpe.selectorLabels" -}}
app.kubernetes.io/name: {{ include "icpe.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Controller-specific labels
*/}}
{{- define "icpe.controller.labels" -}}
{{ include "icpe.labels" . }}
app.kubernetes.io/component: controller
{{- end }}

{{/*
Envoy-specific labels
*/}}
{{- define "icpe.envoy.labels" -}}
{{ include "icpe.labels" . }}
app.kubernetes.io/component: envoy
{{- end }}

{{/*
ServiceAccount name for the controller
*/}}
{{- define "icpe.serviceAccountName" -}}
{{- if .Values.controller.serviceAccount.create }}
{{- default (printf "%s-controller" (include "icpe.fullname" .)) .Values.controller.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.controller.serviceAccount.name }}
{{- end }}
{{- end }}
