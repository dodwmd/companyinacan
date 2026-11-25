{{/* Environment shared by the migrate init container and the app (Listmonk reads LISTMONK_<section>__<key>). */}}
{{- define "newsletter.env" -}}
- {name: LISTMONK_app__address, value: "0.0.0.0:{{ .Values.service.port }}"}
- {name: LISTMONK_db__ssl_mode, value: {{ .Values.database.sslMode | quote }}}
- name: LISTMONK_db__host
  valueFrom: {secretKeyRef: {name: {{ .Values.secrets.database }}, key: host}}
- name: LISTMONK_db__port
  valueFrom: {secretKeyRef: {name: {{ .Values.secrets.database }}, key: port}}
- name: LISTMONK_db__database
  valueFrom: {secretKeyRef: {name: {{ .Values.secrets.database }}, key: name}}
- name: LISTMONK_db__user
  valueFrom: {secretKeyRef: {name: {{ .Values.secrets.database }}, key: user}}
- name: LISTMONK_db__password
  valueFrom: {secretKeyRef: {name: {{ .Values.secrets.database }}, key: password}}
- name: LISTMONK_ADMIN_USER
  valueFrom: {secretKeyRef: {name: {{ .Values.secrets.module }}, key: admin-user}}
- name: LISTMONK_ADMIN_PASSWORD
  valueFrom: {secretKeyRef: {name: {{ .Values.secrets.module }}, key: admin-password}}
{{- end -}}
