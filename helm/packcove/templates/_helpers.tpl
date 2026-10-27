{{- define "packcove.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "packcove.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := include "packcove.name" . -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "packcove.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end -}}

{{- define "packcove.selectorLabels" -}}
app.kubernetes.io/name: {{ include "packcove.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "packcove.labels" -}}
{{- $defaults := dict
  "helm.sh/chart" (printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_")
  "app.kubernetes.io/name" (include "packcove.name" .)
  "app.kubernetes.io/instance" .Release.Name
  "app.kubernetes.io/managed-by" .Release.Service
-}}
{{- if .Chart.AppVersion -}}
{{- $_ := set $defaults "app.kubernetes.io/version" .Chart.AppVersion -}}
{{- end -}}
{{- toYaml (mergeOverwrite (deepCopy (.Values.commonLabels | default dict)) $defaults) -}}
{{- end -}}

{{- define "packcove.image" -}}
{{- $registry := default .Values.image.registry .Values.global.imageRegistry -}}
{{- $prefix := ternary "" (printf "%s/" $registry) (empty $registry) -}}
{{- if .Values.image.digest -}}
{{- printf "%s%s@%s" $prefix .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s%s:%s" $prefix .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{- define "packcove.configName" -}}
{{- printf "%s-config" (include "packcove.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "packcove.dataClaimName" -}}
{{- if .Values.persistence.existingClaim -}}
{{- .Values.persistence.existingClaim -}}
{{- else -}}
{{- printf "%s-data" (include "packcove.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
