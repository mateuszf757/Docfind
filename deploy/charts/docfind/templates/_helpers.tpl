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

{{/*
Nazwa ConfigMapy z sumą treści konfiguracji (decyzja 63). Nowa konfiguracja
to nowa ConfigMapa: stary pod restartowany w trakcie rolloutu albo po helm
rollback czyta swoją, a nie klucz, którego jego obraz nie zna (extra=forbid
odmówiłby startu). Zmiana nazwy w specyfikacji poda sama wywołuje rollout.
*/}}
{{- define "docfind.api.configName" -}}
{{- printf "%s-config-%s" (include "docfind.api.fullname" .) (toYaml .Values.api.config | sha256sum | trunc 10) -}}
{{- end -}}

{{/*
X — najdłuższe zakończenie poda: preStop, dokańczanie żądań i 1 s zapasu.
Bramka drainu porównuje z nim zmierzone zakończenie każdego eksmitowanego
poda (decyzja 58); okres łaski musi je pokryć (validateGracePeriod).
*/}}
{{- define "docfind.api.terminationBudget" -}}
{{- add .Values.api.preStopSleepSeconds (.Values.api.config.service.shutdown_grace_seconds | int) 1 -}}
{{- end -}}

{{/*
Proxy musi zamykać bezczynne połączenia do backendu wcześniej niż backend.
Inaczej proxy wysyła żądanie połączeniem, które uvicorn właśnie zamyka,
i zwraca klientowi błąd — sporadyczny, a więc najtrudniejszy do znalezienia.
*/}}
{{- define "docfind.route.validateIdleTimeout" -}}
{{- if and .Values.route.enabled .Values.route.envoyGatewayPolicies -}}
{{- $proxy := int .Values.route.upstreamIdleTimeoutSeconds -}}
{{- $backend := int .Values.api.config.service.keep_alive_seconds -}}
{{- if ge $proxy $backend -}}
{{- fail (printf "route.upstreamIdleTimeoutSeconds=%d musi być krótszy niż api.config.service.keep_alive_seconds=%d — inaczej proxy trafia w połączenia zamykane przez uvicorn" $proxy $backend) -}}
{{- end -}}
{{- end -}}
{{- end -}}
