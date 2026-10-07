{{/*
Expand the name of the chart.
*/}}
{{- define "krakend-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "krakend-operator.fullname" -}}
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
{{- define "krakend-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "krakend-operator.labels" -}}
helm.sh/chart: {{ include "krakend-operator.chart" . }}
{{ include "krakend-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "krakend-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "krakend-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: controller-manager
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "krakend-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.name }}
{{- .Values.serviceAccount.name }}
{{- else }}
{{- include "krakend-operator.fullname" . }}-controller-manager
{{- end }}
{{- end }}

{{/*
Operator image
*/}}
{{- define "krakend-operator.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}

{{/*
Webhook CA bundle for clientConfig.caBundle when cert-manager does not inject
one. Accepts the PEM bundle itself or its base64 encoding and always emits the
base64 encoding of the PEM on one line, which is what the API server expects.
*/}}
{{- define "krakend-operator.webhookCABundle" -}}
{{- $ca := .Values.webhooks.caBundle | default "" | trim -}}
{{- if contains "-----BEGIN" $ca -}}
{{- $ca | b64enc -}}
{{- else -}}
{{- $ca | nospace -}}
{{- end -}}
{{- end }}

{{/*
OTEL_RESOURCE_ATTRIBUTES from a map: key=value pairs, sorted by key, joined
with commas.
*/}}
{{- define "krakend-operator.resourceAttributes" -}}
{{- $pairs := list -}}
{{- range $key, $value := . -}}
{{- $pairs = append $pairs (printf "%s=%s" $key (toString $value)) -}}
{{- end -}}
{{- join "," $pairs -}}
{{- end }}

{{/*
The OTLP endpoint: telemetry.otlp.endpoint, or the collector on the node's IP
when telemetry.otlp.nodeCollector.enabled. Empty when neither is set.
*/}}
{{- define "krakend-operator.otlpEndpoint" -}}
{{- $otlp := .Values.telemetry.otlp -}}
{{- if and $otlp.nodeCollector.enabled $otlp.endpoint -}}
{{- fail "telemetry.otlp.endpoint and telemetry.otlp.nodeCollector.enabled are both set; use one" -}}
{{- end -}}
{{- if $otlp.nodeCollector.enabled -}}
{{- printf "http://$(NODE_IP):%v" $otlp.nodeCollector.port -}}
{{- else -}}
{{- $otlp.endpoint -}}
{{- end -}}
{{- end }}
