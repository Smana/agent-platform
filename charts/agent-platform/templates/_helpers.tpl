{{- define "ap.labels" -}}
app.kubernetes.io/name: room-broker
app.kubernetes.io/part-of: agent-platform
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{/* The image by digest: a bare tag can move under a running broker. e2e.unpinnedImages lifts it for kind-loaded images. */}}
{{- define "ap.image" -}}
{{- $tag := required "image.tag is <version>@sha256:<digest>" .image.tag -}}
{{- if and (not .root.Values.e2e.unpinnedImages) (not (regexMatch "^[A-Za-z0-9_.-]+@sha256:[0-9a-f]+$" $tag)) -}}
{{- fail "image.tag is <version>@sha256:<digest>, never a bare tag" -}}
{{- end -}}
{{ .image.repository }}:{{ $tag }}
{{- end -}}

{{/* A kind-loaded image is not in any registry: never pull it. */}}
{{- define "ap.pullPolicy" -}}
{{- if .Values.e2e.unpinnedImages }}imagePullPolicy: Never{{ end -}}
{{- end -}}

{{/* The run issuer's subject pattern, from `runs` alone (3a I2). */}}
{{- define "ap.runSubPattern" -}}
{{- printf "^system:serviceaccount:%s:%s([a-z2-7]{8})$" .Values.runs.namespace .Values.runs.namePrefix -}}
{{- end -}}

{{- define "ap.local" -}}
{{- if eq (.Values.broker.human.mode | default "oidc") "local" }}true{{ end -}}
{{- end -}}

{{- define "ap.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities: {drop: [ALL]}
{{- end -}}
