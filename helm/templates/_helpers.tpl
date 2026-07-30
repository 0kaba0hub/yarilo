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
{{- define "yarilo.adminBackendEnv" -}}
{{- $tokenSecret := .Values.components.backendAPI.token_secret }}
{{- if eq $tokenSecret "" }}
{{- $tokenSecret = printf "%s-backend-api-token" (include "yarilo.fullname" .) }}
{{- end }}
- name: YARILO_ADMIN_TYPE
  value: backend
- name: YARILO_API_URL
  {{- /* Co-located: backend-api runs in THIS pod on the pod IP (no separate
         Service), so reach it over localhost. Legacy: the -backend-api Service. */}}
  {{- if .Values.components.backend.coLocated }}
  value: "http://localhost:9105"
  {{- else }}
  value: {{ printf "http://%s-backend-api:9105" (include "yarilo.fullname" .) }}
  {{- end }}
- name: YARILO_API_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ $tokenSecret }}
      key: token
{{- end }}
{{/*
YARILO_ADMIN_URL / YARILO_ADMIN_TOKEN env block for the director admin API.
Read directly by yarilo-admin's director subcommands regardless of
YARILO_ADMIN_TYPE (#755) — distinct from yarilo.adminBackendEnv's
YARILO_API_URL/YARILO_API_TOKEN pair, which is claimed by the backend plane.
*/}}
{{- define "yarilo.adminDirectorEnv" -}}
{{- $tokenSecret := printf "%s-director-api-token" (include "yarilo.fullname" .) }}
- name: YARILO_ADMIN_URL
  value: {{ printf "http://%s-director-api:%v" (include "yarilo.fullname" .) .Values.components.director.api.port }}
- name: YARILO_ADMIN_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ $tokenSecret }}
      key: token
{{- end }}

{{/*
yarilo.backendVolumeMounts — the volume mounts shared by every co-located
backend protocol/fts/backend-api container (#788): config, tmp, the shared
readiness emptyDir, the mail PV, and the optional internal-mTLS secret.
Args: dict "root" $ "itls" <internalTLS config>.
*/}}
{{- define "yarilo.backendVolumeMounts" -}}
{{- $root := .root -}}
{{- $readyDir := $root.Values.backend_register.readiness_dir | default "/run/yarilo-ready" -}}
- name: config
  mountPath: /etc/yarilo
  readOnly: true
- name: tmp
  mountPath: /tmp
- name: ready
  mountPath: {{ $readyDir }}
{{- if $root.Values.storage.persistence.enabled }}
- name: mail
  mountPath: {{ $root.Values.storage.maildir_root | default "/var/mail/vhosts" }}
{{- end }}
{{- include "yarilo.internalTLSMount" .itls }}
{{- end -}}

{{/*
Graceful-shutdown preStop hook (#857): delay SIGTERM so kube removes the pod
from Service endpoints before the process stops accepting, closing the
racing-new-connection window. Pass the ROOT context.
*/}}
{{- define "yarilo.preStopDrain" -}}
lifecycle:
  preStop:
    exec:
      command: ["/bin/sh", "-c", "sleep {{ .Values.gracefulShutdown.preStopSleepSeconds | default 5 }}"]
{{- end -}}

{{/*
Effective log level for one component (#887 follow-up).

Falls back to the installation-wide .Values.logLevel unless the component's name
appears in .Values.logLevelOverrides. Keyed by the component name the container
already reports in YARILO_COMPONENT, so an operator raises verbosity for a single
service without knowing the internal values.yaml key layout:

  logLevelOverrides:
    yarilo-auth: debug

Call: (dict "name" "yarilo-auth" "root" $)
*/}}
{{- define "yarilo.logLevel" -}}
{{- $ovr := .root.Values.logLevelOverrides | default dict -}}
{{- if hasKey $ovr .name -}}
{{- index $ovr .name -}}
{{- else -}}
{{- .root.Values.logLevel -}}
{{- end -}}
{{- end -}}

{{/*
startupProbe for a login pod, replacing its wait-* init containers (#903).

Safe for login pods specifically: none of them dials auth or anvil during startup
— those connections are created lazily on the first login (#885/#891) — so the
process comes up regardless and the probe only withholds traffic until the
dependencies answer.

While it fails the pod stays out of the Service endpoints AND liveness is not run,
so a dependency slow to appear cannot cause a restart. It stops after the first
success: a dependency failing later is a runtime error the login reports to the
client, not a reason to pull the pod.

URLs are POSITIONAL. yarctl registers a global --url (Director API) and strips
global flags from argv before a subcommand sees them, so a --url here would be
swallowed and the probe would never pass (#906).

Call: (dict "probe" .Values.components.<c>.startupProbe "root" $ "auth" true "anvil" true)
*/}}
{{- define "yarilo.loginStartupProbe" -}}
{{- $p := .probe -}}
{{- $root := .root -}}
{{- if $p.enabled }}
startupProbe:
  exec:
    command:
      - yarctl
      - wait
      - --timeout={{ $p.timeout }}
      {{- if .auth }}
      - http://{{ include "yarilo.fullname" $root }}-auth-telemetry.{{ $root.Release.Namespace }}.svc:8080/readyz
      {{- end }}
      {{- if and .anvil $root.Values.components.anvil.enabled }}
      - http://{{ include "yarilo.fullname" $root }}-anvil-telemetry.{{ $root.Release.Namespace }}.svc:8080/readyz
      {{- end }}
  periodSeconds: {{ $p.periodSeconds }}
  ## failureThreshold x periodSeconds is the whole startup budget. Keep it
  ## generous: exceeding it restarts a pod that is healthy and merely waiting,
  ## which the init container never did.
  failureThreshold: {{ $p.failureThreshold }}
{{- end }}
{{- end -}}

{{/*
startupProbe that waits on raw dependency endpoints via `yarctl wait`, replacing a
backend/shared pod's wait-* init containers (#903). Unlike yarilo.loginStartupProbe
(which targets the auth/anvil telemetry /readyz URLs), this takes an explicit list of
targets so a pod can wait on tcp://<db> / tcp://<redis> that have no HTTP endpoint.

The pod must come up WITHOUT dialing the dependency at startup (lazy client, no
os.Exit), or removing the init container swaps clean waiting for CrashLoopBackOff.

Call: (dict "probe" .Values.components.<c>.startupProbe "targets" (list "tcp://host:port" ...))
*/}}
{{- define "yarilo.depStartupProbe" -}}
{{- $p := .probe -}}
{{- if $p.enabled }}
startupProbe:
  exec:
    command:
      - yarctl
      - wait
      - --timeout={{ $p.timeout }}
      {{- range .targets }}
      - {{ . }}
      {{- end }}
  periodSeconds: {{ $p.periodSeconds }}
  ## failureThreshold x periodSeconds is the whole startup budget. Keep it generous:
  ## exceeding it restarts a pod that is healthy and merely waiting for a dependency.
  failureThreshold: {{ $p.failureThreshold }}
{{- end }}
{{- end -}}
