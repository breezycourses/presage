{{- define "presage.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "presage.fullname" -}}
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

{{- define "presage.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "presage.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "presage.selectorLabels" -}}
app.kubernetes.io/name: {{ include "presage.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "presage.controllerSelectorLabels" -}}
{{ include "presage.selectorLabels" . }}
app.kubernetes.io/component: controller
{{- end -}}

{{- define "presage.forecasterSelectorLabels" -}}
{{ include "presage.selectorLabels" . }}
app.kubernetes.io/component: forecaster
{{- end -}}

{{- define "presage.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "presage.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "presage.controllerImage" -}}
{{- printf "%s:%s" .Values.controller.image.repository (default .Chart.AppVersion .Values.controller.image.tag) -}}
{{- end -}}

{{- define "presage.forecasterImage" -}}
{{- printf "%s:%s" .Values.forecaster.image.repository (default .Chart.AppVersion .Values.forecaster.image.tag) -}}
{{- end -}}
