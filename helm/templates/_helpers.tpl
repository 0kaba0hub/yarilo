{{- define "yarilo.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

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

{{- define "yarilo.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "yarilo.labels" -}}
helm.sh/chart: {{ include "yarilo.chart" . }}
app.kubernetes.io/name: {{ include "yarilo.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: yarilo
{{- end }}

{{/* Call with: (dict "root" . "component" "director") */}}
{{- define "yarilo.componentSelectorLabels" -}}
app.kubernetes.io/name: {{ include "yarilo.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{- define "yarilo.componentLabels" -}}
helm.sh/chart: {{ include "yarilo.chart" .root }}
{{ include "yarilo.componentSelectorLabels" . }}
app.kubernetes.io/version: {{ .root.Values.image.tag | default .root.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/part-of: yarilo
{{- end }}

{{- define "yarilo.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{- define "yarilo.configChecksum" -}}
checksum/config: {{ include (print $.Template.BasePath "/configmap.yaml") . | sha256sum }}
{{- end }}

{{/*
External TLS volume (client-facing cert, e.g. IMAPS/POP3S on director).
Mounted at /etc/yarilo/tls. Call with secretName string.
*/}}
{{- define "yarilo.externalTLSVolume" -}}
{{- if . }}
- name: tls
  secret:
    secretName: {{ . }}
    optional: false
{{- end }}
{{- end }}

{{- define "yarilo.externalTLSMount" -}}
{{- if . }}
- name: tls
  mountPath: /etc/yarilo/tls
  readOnly: true
{{- end }}
{{- end }}

{{/*
Internal mTLS volume (inter-component: director↔auth, director↔backend).
Mounted at /etc/yarilo/internal-tls.
Call with component internalTLS config: (dict "enabled" true "secretName" "...")
*/}}
{{- define "yarilo.internalTLSVolume" -}}
{{- if and .enabled .secretName }}
- name: internal-tls
  secret:
    secretName: {{ .secretName }}
    optional: false
{{- end }}
{{- end }}

{{- define "yarilo.internalTLSMount" -}}
{{- if and .enabled .secretName }}
- name: internal-tls
  mountPath: /etc/yarilo/internal-tls
  readOnly: true
{{- end }}
{{- end }}

{{/*
Init container that blocks until a TCP host:port accepts connections.
Call: (dict "name" "wait-foo" "host" "hostname" "port" "6379" "image" "busybox:1.36")
*/}}
{{- define "yarilo.initWaitTCP" -}}
- name: {{ .name }}
  image: {{ .image }}
  command:
    - sh
    - -c
    - until nc -z {{ .host | quote }} {{ .port }}; do echo "waiting for {{ .host }}:{{ .port }}"; sleep 2; done
  securityContext:
    allowPrivilegeEscalation: false
    runAsNonRoot: true
    runAsUser: 65534
    seccompProfile:
      type: RuntimeDefault
    capabilities:
      drop: ["ALL"]
{{- end }}

{{/*
Redis hostname for init-container TCP probe.
Bundled → internal ClusterIP DNS; external → parsed from redis.externalUrl.
*/}}
{{- define "yarilo.redisInitHost" -}}
{{- if .Values.redis.bundled -}}
{{- printf "%s-redis.%s.svc.cluster.local" (include "yarilo.fullname" .) .Release.Namespace -}}
{{- else -}}
{{- $u := urlParse .Values.redis.externalUrl -}}
{{- index (splitList ":" $u.host) 0 -}}
{{- end -}}
{{- end }}

{{/*
Redis port for init-container TCP probe.
*/}}
{{- define "yarilo.redisInitPort" -}}
{{- if .Values.redis.bundled -}}
6379
{{- else -}}
{{- $u := urlParse .Values.redis.externalUrl -}}
{{- index (splitList ":" $u.host) 1 -}}
{{- end -}}
{{- end }}

{{/*
Database hostname for init-container TCP probe.
Set database.initAddr to "host:port" to enable. Returns empty when not set.
*/}}
{{- define "yarilo.dbInitHost" -}}
{{- if (.Values.database | default dict).initAddr -}}
{{- index (splitList ":" .Values.database.initAddr) 0 -}}
{{- end -}}
{{- end }}

{{/*
Database port for init-container TCP probe.
*/}}
{{- define "yarilo.dbInitPort" -}}
{{- if (.Values.database | default dict).initAddr -}}
{{- index (splitList ":" .Values.database.initAddr) 1 -}}
{{- end -}}
{{- end }}

{{/*
YARILO_DB_DSN env block — injects DSN from Secret or literal value.
Include in any component that reads passdb/userdb from SQL.
*/}}
{{- define "yarilo.dbEnv" -}}
{{- if (.Values.database | default dict).secretName -}}
- name: YARILO_DB_DSN
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.secretName }}
      key: {{ .Values.database.secretKey | default "dsn" }}
{{- else if (.Values.database | default dict).dsn -}}
- name: YARILO_DB_DSN
  value: {{ .Values.database.dsn | quote }}
{{- end -}}
{{- end }}
