{{/*
Expand the name of the chart.
*/}}
{{- define "node-rotation-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this
(by the DNS naming spec). If release name contains chart name it will be used
as a full name.
*/}}
{{- define "node-rotation-controller.fullname" -}}
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
Create chart name and version as used by the chart label.
*/}}
{{- define "node-rotation-controller.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "node-rotation-controller.labels" -}}
helm.sh/chart: {{ include "node-rotation-controller.chart" . }}
{{ include "node-rotation-controller.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "node-rotation-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "node-rotation-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
The name of the service account to use.
*/}}
{{- define "node-rotation-controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "node-rotation-controller.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The PriorityClass name for the surge placeholder Pod. Not namespaced/prefixed by
release: it is a cluster-scoped object and must match the controller's
--priority-class flag exactly (spec §3.3).
*/}}
{{- define "node-rotation-controller.placeholderPriorityClassName" -}}
{{- .Values.placeholder.priorityClass.name }}
{{- end }}
