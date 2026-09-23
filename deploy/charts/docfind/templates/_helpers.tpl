{{/* Nazwa bazowa zasobów: nazwa release'u, chyba że już zawiera nazwę charta. */}}
{{- define "docfind.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "docfind.api.fullname" -}}
{{- printf "%s-api" (include "docfind.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Etykiety selektora są niezmienne przez całe życie Deploymentu — zmiana
selektora wymaga usunięcia i ponownego utworzenia obiektu. Dlatego nie ma
w nich wersji ani nazwy charta.
*/}}
{{- define "docfind.api.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: api
{{- end -}}

{{- define "docfind.api.labels" -}}
{{ include "docfind.api.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.api.image.tag | quote }}
app.kubernetes.io/part-of: docfind
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/*
Zabezpieczenie przed zbyt krótkim okresem łaski: preStop plus dokańczanie
żądań musi się zmieścić, zanim kubelet wyśle SIGKILL.
*/}}
{{- define "docfind.api.validateGracePeriod" -}}
{{- $needed := add .Values.api.preStopSleepSeconds (.Values.api.config.service.shutdown_grace_seconds | int) 1 -}}
{{- if lt (int .Values.api.terminationGracePeriodSeconds) (int $needed) -}}
{{- fail (printf "api.terminationGracePeriodSeconds=%v jest za krótki: preStop (%v s) + dokańczanie żądań (%v s) + 1 s zapasu wymaga co najmniej %v s" .Values.api.terminationGracePeriodSeconds .Values.api.preStopSleepSeconds .Values.api.config.service.shutdown_grace_seconds $needed) -}}
{{- end -}}
{{- end -}}
