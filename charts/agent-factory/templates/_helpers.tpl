{{- define "agent-factory.labels" -}}
app.kubernetes.io/name: agent-factory
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{/* The image by digest: a bare tag can move under a running factory. */}}
{{- define "agent-factory.image" -}}
{{- $tag := required "image.tag is <version>@sha256:<digest>" .Values.image.tag -}}
{{- if not (regexMatch "^[A-Za-z0-9_.-]+@sha256:[0-9a-f]+$" $tag) -}}
{{- fail "image.tag is <version>@sha256:<digest>, never a bare tag" -}}
{{- end -}}
{{ .Values.image.repository }}:{{ $tag }}
{{- end -}}
