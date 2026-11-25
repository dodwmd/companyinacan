{{/* Environment of the EspoCRM containers (variable names from the Docker Official Image README). */}}
{{- define "crm.env" -}}
- {name: ESPOCRM_DATABASE_PLATFORM, value: Mysql}
- name: ESPOCRM_DATABASE_HOST
  valueFrom: {secretKeyRef: {name: {{ .Values.secrets.database }}, key: host}}
- name: ESPOCRM_DATABASE_PORT
  valueFrom: {secretKeyRef: {name: {{ .Values.secrets.database }}, key: port}}
- name: ESPOCRM_DATABASE_NAME
  valueFrom: {secretKeyRef: {name: {{ .Values.secrets.database }}, key: name}}
- name: ESPOCRM_DATABASE_USER
  valueFrom: {secretKeyRef: {name: {{ .Values.secrets.database }}, key: user}}
- name: ESPOCRM_DATABASE_PASSWORD
  valueFrom: {secretKeyRef: {name: {{ .Values.secrets.database }}, key: password}}
- name: ESPOCRM_ADMIN_USERNAME
  valueFrom: {secretKeyRef: {name: {{ .Values.secrets.module }}, key: admin-user}}
- name: ESPOCRM_ADMIN_PASSWORD
  valueFrom: {secretKeyRef: {name: {{ .Values.secrets.module }}, key: admin-password}}
- {name: ESPOCRM_SITE_URL, value: "{{ include "mod.scheme" . }}://{{ .Values.ingress.host }}"}
{{- end -}}

{{/* One volume, three subPaths: the directories the image documents as volumes. */}}
{{- define "crm.mounts" -}}
- {name: data, mountPath: {{ .Values.persistence.mountPath }}/data, subPath: data}
- {name: data, mountPath: {{ .Values.persistence.mountPath }}/custom, subPath: custom}
- {name: data, mountPath: {{ .Values.persistence.mountPath }}/client/custom, subPath: client-custom}
{{- end -}}
