{{- define "platform.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: docfind
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/*
Wydawca Let's Encrypt wybrany dla Gateway musi istnieć — a istnieje tylko
z adresem e-mail i strefą. Bez tej kontroli Gateway czekałby na certyfikat
od nieistniejącego wydawcy i listener HTTPS po cichu by nie wstał.
*/}}
{{- define "platform.validateIssuer" -}}
{{- $acme := and .Values.acme.email .Values.acme.dnsZone -}}
{{- if and (hasPrefix "letsencrypt" .Values.gateway.issuer) (not $acme) -}}
{{- fail (printf "gateway.issuer=%s wymaga acme.email i acme.dnsZone — bez nich wydawcy Let's Encrypt nie powstają" .Values.gateway.issuer) -}}
{{- end -}}
{{- if and $acme (hasPrefix "letsencrypt" .Values.gateway.issuer) (not (hasSuffix (printf ".%s" .Values.acme.dnsZone) .Values.gateway.hostname)) -}}
{{- fail (printf "gateway.hostname=%s nie należy do strefy acme.dnsZone=%s — wyzwanie DNS-01 nie miałoby gdzie powstać" .Values.gateway.hostname .Values.acme.dnsZone) -}}
{{- end -}}
{{- end -}}
