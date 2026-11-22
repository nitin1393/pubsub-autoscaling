{{- define "pubsub-hpa.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "pubsub-hpa.authName" -}}
{{ .Values.scaledObjectName }}-{{ .source | lower }}
{{- end -}}

{{/* Validate the configuration early with clear messages. */}}
{{- define "pubsub-hpa.validate" -}}
{{- $src := .Values.metricsSource -}}
{{- if not (has $src (list "gcpPubsub" "groundcover" "prometheus")) -}}
{{- fail (printf "metricsSource must be gcpPubsub, groundcover or prometheus (got %q)" $src) -}}
{{- end -}}
{{- if and (eq $src "gcpPubsub") (contains "MY_PROJECT" .Values.gcpPubsub.subscription) -}}
{{- fail "set gcpPubsub.subscription to projects/<project>/subscriptions/<subscription>" -}}
{{- end -}}
{{- if lt (int .Values.replicas.max) (int .Values.replicas.min) -}}
{{- fail "replicas.max must be >= replicas.min" -}}
{{- end -}}
{{- if not (or .Values.triggers.backlog.enabled .Values.triggers.oldestAge.enabled .Values.triggers.inFlight.enabled) -}}
{{- fail "enable at least one trigger" -}}
{{- end -}}
{{- $np := .Values.nodePool -}}
{{- if and $np.enabled (not (has $np.provisioner (list "autoMode" "karpenter"))) -}}
{{- fail (printf "nodePool.provisioner must be autoMode or karpenter (got %q)" $np.provisioner) -}}
{{- end -}}
{{- if and $np.enabled (eq $np.provisioner "karpenter") $np.karpenter.createEC2NodeClass (or (contains "my-cluster" $np.karpenter.ec2NodeClass.role) (eq $np.karpenter.ec2NodeClass.discoveryTag "my-cluster")) -}}
{{- fail "set nodePool.karpenter.ec2NodeClass.role and discoveryTag for your cluster (or point nodeClassName at an existing EC2NodeClass)" -}}
{{- end -}}
{{- if and .Values.triggers.inFlight.enabled (not (has .Values.triggers.inFlight.source (list "groundcover" "prometheus"))) -}}
{{- fail "triggers.inFlight.source must be groundcover or prometheus" -}}
{{- end -}}
{{- end -}}

{{/* "true" if a source needs a TriggerAuthentication. */}}
{{- define "pubsub-hpa.needsAuth" -}}
{{- $v := .Values -}}
{{- if or (eq .source "gcpPubsub") (eq .source "groundcover") -}}true
{{- else if and (eq .source "prometheus") $v.prometheus.existingSecret (has $v.prometheus.authModes (list "bearer" "basic")) -}}true
{{- end -}}
{{- end -}}

{{/* A prometheus-type trigger. Args: dict root, name, query, threshold, activation, source */}}
{{- define "pubsub-hpa.promTrigger" -}}
{{- $v := .root.Values -}}
- type: prometheus
  name: {{ .name }}
  {{- if include "pubsub-hpa.needsAuth" (dict "Values" $v "source" .source) }}
  authenticationRef:
    name: {{ include "pubsub-hpa.authName" (dict "Values" $v "source" .source) }}
  {{- end }}
  metadata:
    {{- if eq .source "groundcover" }}
    serverAddress: {{ $v.groundcover.serverAddress | quote }}
    authModes: bearer
    {{- with $v.groundcover.backendId }}
    customHeaders: {{ printf "X-Backend-Id=%s" . | quote }}
    {{- end }}
    {{- else }}
    serverAddress: {{ $v.prometheus.serverAddress | quote }}
    {{- with $v.prometheus.authModes }}
    authModes: {{ . | quote }}
    {{- end }}
    {{- end }}
    query: {{ .query | quote }}
    threshold: {{ .threshold | toString | quote }}
    {{- if ne (toString .activation) "" }}
    activationThreshold: {{ .activation | toString | quote }}
    {{- end }}
    ignoreNullValues: "false"
{{- end -}}
