{{/*
Expand the name of the chart.
*/}}
{{- define "yarilo.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "yarilo.fullname" -}}
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
Chart label.
*/}}
{{- define "yarilo.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "yarilo.labels" -}}
helm.sh/chart: {{ include "yarilo.chart" . }}
{{ include "yarilo.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "yarilo.selectorLabels" -}}
app.kubernetes.io/name: {{ include "yarilo.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Image reference — tag defaults to appVersion.
*/}}
{{- define "yarilo.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/*
Name of the TLS secret (imap.tls.secretName or generated name).
*/}}
{{- define "yarilo.tlsSecretName" -}}
{{- if .Values.imap.tls.secretName }}
{{- .Values.imap.tls.secretName }}
{{- else }}
{{- include "yarilo.fullname" . }}-tls
{{- end }}
{{- end }}

{{/*
Config checksum annotation — forces pod restart on ConfigMap change.
*/}}
{{- define "yarilo.configChecksum" -}}
checksum/config: {{ include (print $.Template.BasePath "/configmap.yaml") . | sha256sum }}
{{- end }}
