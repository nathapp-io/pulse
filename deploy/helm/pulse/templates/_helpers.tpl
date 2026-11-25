{{/* Common helpers. */}}

{{- define "pulse.name" -}}
{{- default "pulse" .Chart.Name -}}
{{- end -}}

{{- define "pulse.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "pulse.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "pulse.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 }}
app.kubernetes.io/name: {{ include "pulse.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "pulse.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pulse.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "pulse.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "pulse.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "pulse.viewerImage" -}}
{{- printf "%s:%s" .Values.viewer.image.repository (default .Chart.AppVersion .Values.viewer.image.tag) -}}
{{- end -}}

{{- define "pulse.proberImage" -}}
{{- printf "%s:%s" .Values.prober.image.repository (default .Chart.AppVersion .Values.prober.image.tag) -}}
{{- end -}}

{{- define "pulse.viewerServiceName" -}}
{{- printf "%s-viewer" (include "pulse.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "pulse.viewerSecretName" -}}
{{- default (printf "%s-auth" (include "pulse.fullname" .)) .Values.viewer.auth.existingSecret -}}
{{- end -}}

{{/* Keep the legacy combined Secret name for managed ingest credentials so
     lookup reuses the shared token on upgrade. Never derive it from auth. */}}
{{- define "pulse.managedIngestSecretName" -}}
{{- printf "%s-viewer" (include "pulse.fullname" .) -}}
{{- end -}}

{{/* Non-empty only when we own the viewer's ingest Secret: inline bound
     tokens, or shared mode without an external Secret. */}}
{{- define "pulse.managesIngest" -}}
{{- with .Values.viewer.ingest -}}
{{- if and (not .existingTokensSecret) (or .tokens (not .existingSecret)) -}}yes{{- end -}}
{{- end -}}
{{- end -}}

{{- define "pulse.proberSecretName" -}}
{{- default (include "pulse.managedIngestSecretName" .) .Values.viewer.ingest.existingSecret -}}
{{- end -}}

{{/* Expected probers default to the chart's own prober, so MISSING means
     "the prober I deployed is not reporting" rather than "empty list". */}}
{{- define "pulse.expectedProbers" -}}
{{- if .Values.viewer.expectedProbers -}}
{{- join "," .Values.viewer.expectedProbers -}}
{{- else if gt (int .Values.prober.replicaCount) 0 -}}
{{- .Values.prober.name -}}
{{- end -}}
{{- end -}}

{{/* Fail early on a combination that corrupts history rather than warning later. */}}
{{- define "pulse.validate" -}}
{{- if and (eq .Values.viewer.store "sqlite") (gt (int .Values.viewer.replicaCount) 1) -}}
{{- fail "viewer.replicaCount must be 1 with the sqlite store: two pods writing one database corrupt history. Use store=memory (history lost on restart) or run one viewer." -}}
{{- end -}}
{{- if and .Values.viewer.persistence.enabled (eq .Values.viewer.store "memory") -}}
{{- fail "viewer.persistence.enabled is true but viewer.store is memory, which keeps nothing on disk. Set persistence.enabled=false or store=sqlite." -}}
{{- end -}}
{{- end -}}

{{/* A managed name must never alias any externally supplied Secret, even if
     it serves a different role. Validation doesn't require cluster access. */}}
{{- define "pulse.validateSecrets" -}}
{{- $auth := .Values.viewer.auth -}}
{{- $ingest := .Values.viewer.ingest -}}
{{- $external := list $auth.existingSecret $ingest.existingSecret $ingest.existingTokensSecret -}}
{{- if and (not $auth.existingSecret) (has (include "pulse.viewerSecretName" .) $external) -}}
{{- fail "an existing Secret name collides with the chart-managed auth Secret; use a distinct external name" -}}
{{- end -}}
{{- if and (include "pulse.managesIngest" .) (has (include "pulse.managedIngestSecretName" .) $external) -}}
{{- fail "an existing Secret name collides with the chart-managed ingest Secret; use a distinct external name" -}}
{{- end -}}
{{- if and (or $ingest.tokens $ingest.existingTokensSecret) (not $ingest.existingSecret) (gt (int .Values.prober.replicaCount) 0) -}}
{{- fail "bound and file modes require viewer.ingest.existingSecret containing PULSE_INGEST_TOKEN for the chart prober, or prober.replicaCount=0" -}}
{{- end -}}
{{- end -}}
