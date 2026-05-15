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
Selector labels (legacy — used by resources that have no component context).
*/}}
{{- define "yarilo.selectorLabels" -}}
app.kubernetes.io/name: {{ include "yarilo.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Component-aware selector labels.
Call with: (dict "root" . "component" "director")
*/}}
{{- define "yarilo.componentSelectorLabels" -}}
app.kubernetes.io/name: {{ include "yarilo.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/*
Component-aware full labels (includes chart, version, part-of).
Call with: (dict "root" . "component" "director")
*/}}
{{- define "yarilo.componentLabels" -}}
helm.sh/chart: {{ include "yarilo.chart" .root }}
{{ include "yarilo.componentSelectorLabels" . }}
app.kubernetes.io/version: {{ .root.Values.image.tag | default .root.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/part-of: yarilo
{{- end }}

{{/*
Image reference — tag defaults to appVersion.
*/}}
{{- define "yarilo.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default "latest" }}
{{- end }}

{{/*
Config checksum annotation — forces pod restart on ConfigMap change.
*/}}
{{- define "yarilo.configChecksum" -}}
checksum/config: {{ include (print $.Template.BasePath "/configmap.yaml") . | sha256sum }}
{{- end }}

{{/*
Internal TLS volume — shared by all component deployments.
Renders nothing when internalTLS.secretName is empty.
*/}}
{{- define "yarilo.internalTLSVolume" -}}
{{- if .Values.internalTLS.secretName }}
- name: internal-tls
  secret:
    secretName: {{ .Values.internalTLS.secretName }}
    optional: false
{{- end }}
{{- end }}

{{/*
Internal TLS volume mount.
*/}}
{{- define "yarilo.internalTLSMount" -}}
{{- if .Values.internalTLS.secretName }}
- name: internal-tls
  mountPath: /etc/yarilo/tls
  readOnly: true
{{- end }}
{{- end }}
