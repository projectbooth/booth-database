{{/*
Standard name/label helpers, the same shape `helm create` scaffolds — mirrors booth-storage's and
booth-core's own chart helpers.
*/}}

{{- define "booth-database.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "booth-database.fullname" -}}
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

{{- define "booth-database.labels" -}}
app.kubernetes.io/name: {{ include "booth-database.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
booth.projectbooth.io/module: database
{{- end -}}

{{- define "booth-database.selectorLabels" -}}
app.kubernetes.io/name: {{ include "booth-database.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "booth-database.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "booth-database.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* The bundled server's Service/StatefulSet name. */}}
{{- define "booth-database.postgresName" -}}
{{- printf "%s-postgres" (include "booth-database.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* The bundled server's admin-password Secret. */}}
{{- define "booth-database.adminSecretName" -}}
{{- printf "%s-postgres-admin" (include "booth-database.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "booth-database.validateMode" -}}
{{- if not (has .Values.mode (list "bundled" "external")) -}}
{{- fail (printf "mode must be \"bundled\" or \"external\", got %q" .Values.mode) -}}
{{- end -}}
{{- end -}}

{{/* Whether the module pins the bundled StatefulSet to its node (ADR 0090). */}}
{{- define "booth-database.pinToNode" -}}
{{- if and (eq .Values.mode "bundled") .Values.bundled.pinToNode -}}true{{- end -}}
{{- end -}}
