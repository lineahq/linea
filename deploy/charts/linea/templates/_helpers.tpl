{{- define "linea.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 50 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "linea.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "linea.serverSelector" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: server
{{- end -}}

{{- define "linea.workerSelector" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: worker
{{- end -}}

{{- define "linea.poolSelector" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: pool
{{- end -}}

{{- define "linea.builderSelector" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: builder
{{- end -}}

{{- define "linea.postgresqlSelector" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: postgresql
{{- end -}}

{{- define "linea.seaweedfsSelector" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: seaweedfs
{{- end -}}

{{- define "linea.builderSecret" -}}
{{- .Values.builder.existingSecret | default (printf "%s-builder-token" (include "linea.fullname" .)) -}}
{{- end -}}

{{- define "linea.workerSecret" -}}
{{- .Values.worker.existingSecret | default (printf "%s-worker-token" (include "linea.fullname" .)) -}}
{{- end -}}

{{- define "linea.fleetSecret" -}}
{{- .Values.fleet.existingSecret | default (printf "%s-fleet" (include "linea.fullname" .)) -}}
{{- end -}}

{{- define "linea.storeSecret" -}}
{{- .Values.store.existingSecret | default (printf "%s-store" (include "linea.fullname" .)) -}}
{{- end -}}

{{- define "linea.oidcSecret" -}}
{{- .Values.oidc.existingSecret | default (printf "%s-oidc" (include "linea.fullname" .)) -}}
{{- end -}}

{{- define "linea.hasOIDCSecret" -}}
{{- if or .Values.oidc.clientSecret .Values.oidc.existingSecret }}true{{ end -}}
{{- end -}}

{{- define "linea.hasFleet" -}}
{{- if .Values.fleet.url }}true{{ end -}}
{{- end -}}

{{/* The server accepts workers when the chart deploys the pool or a worker, or is given a registration token. */}}
{{- define "linea.acceptsWorkers" -}}
{{- if or .Values.pool.enabled .Values.worker.enabled .Values.worker.token .Values.worker.existingSecret }}true{{ end -}}
{{- end -}}

{{/* The server accepts builders when the chart deploys one or is given a build token. */}}
{{- define "linea.acceptsBuilds" -}}
{{- if or .Values.builder.enabled .Values.builder.token .Values.builder.existingSecret }}true{{ end -}}
{{- end -}}

{{/* An autoscaler sets the builder's replicas (builder.autoscaling.kind keda or hpa). */}}
{{- define "linea.builderAutoscaled" -}}
{{- $kind := .Values.builder.autoscaling.kind -}}
{{- if not (has $kind (list "none" "keda" "hpa")) -}}
{{- fail (printf "builder.autoscaling.kind is %q: use none, keda or hpa" $kind) -}}
{{- end -}}
{{- if and .Values.builder.enabled (ne $kind "none") }}true{{ end -}}
{{- end -}}

{{/* The server serves the build metrics (server.metrics.enabled, or builder autoscaling). */}}
{{- define "linea.serverMetrics" -}}
{{- if or .Values.server.metrics.enabled (include "linea.builderAutoscaled" .) }}true{{ end -}}
{{- end -}}

{{/* The most builder pods that run at once. */}}
{{- define "linea.builderMaxReplicas" -}}
{{- if include "linea.builderAutoscaled" . -}}
{{- .Values.builder.autoscaling.maxReplicas -}}
{{- else -}}
{{- .Values.builder.replicas -}}
{{- end -}}
{{- end -}}

{{/* The claim mounted at the builder's /work, or empty for an emptyDir. */}}
{{- define "linea.builderCacheClaim" -}}
{{- $pvc := .Values.builder.cache.persistentVolumeClaim -}}
{{- if $pvc.existingClaim -}}
{{- $pvc.existingClaim -}}
{{- else if $pvc.create -}}
{{- printf "%s-builder-cache" (include "linea.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/* The builder's work volume can be mounted by one pod only. */}}
{{- define "linea.builderCacheExclusive" -}}
{{- if and (include "linea.builderCacheClaim" .) (eq .Values.builder.cache.persistentVolumeClaim.accessMode "ReadWriteOnce") }}true{{ end -}}
{{- end -}}

{{/* Fails on builder scaling values the cluster or the cache cannot serve. */}}
{{- define "linea.checkBuilderScaling" -}}
{{- $a := .Values.builder.autoscaling -}}
{{- if include "linea.builderAutoscaled" . -}}
{{- if lt (int $a.maxReplicas) (max 1 (int $a.minReplicas)) -}}
{{- fail "builder.autoscaling.maxReplicas must be at least minReplicas and at least 1" -}}
{{- end -}}
{{- if lt (int .Values.builder.capacity) 1 -}}
{{- fail "builder.capacity must be at least 1 with builder.autoscaling" -}}
{{- end -}}
{{- if and (eq $a.kind "keda") (not (.Capabilities.APIVersions.Has "keda.sh/v1alpha1")) -}}
{{- fail "builder.autoscaling.kind is keda, but the cluster has no keda.sh/v1alpha1 API: install KEDA first, or use hpa or none" -}}
{{- end -}}
{{- if and (eq $a.kind "hpa") (not (.Capabilities.APIVersions.Has "autoscaling/v2")) -}}
{{- fail "builder.autoscaling.kind is hpa, but the cluster has no autoscaling/v2 API" -}}
{{- end -}}
{{- end -}}
{{- if and (include "linea.builderCacheExclusive" .) (gt (int (include "linea.builderMaxReplicas" .)) 1) -}}
{{- fail "builder.cache.persistentVolumeClaim.accessMode is ReadWriteOnce, which one builder pod can mount: set builder.replicas (or builder.autoscaling.maxReplicas) to 1, or use ReadWriteMany" -}}
{{- end -}}
{{- end -}}

{{/* Builders and workers need different tokens (builds.token_file). */}}
{{- define "linea.checkTokens" -}}
{{- if and .Values.builder.token .Values.worker.token (eq .Values.builder.token .Values.worker.token) -}}
{{- fail "builder.token and worker.token are equal: builders and workers need different tokens" -}}
{{- end -}}
{{- if and .Values.builder.existingSecret .Values.worker.existingSecret (eq .Values.builder.existingSecret .Values.worker.existingSecret) -}}
{{- fail "builder.existingSecret and worker.existingSecret name the same Secret: builders and workers need different tokens" -}}
{{- end -}}
{{- end -}}

{{/* Fails when the pool or a builder runs under a NetworkPolicy that does not reach the object store. */}}
{{- define "linea.checkObjectStorage" -}}
{{- if and .Values.networkPolicy.enabled (not .Values.networkPolicy.objectStorage) (not .Values.seaweedfs.enabled) -}}
{{- fail "networkPolicy.objectStorage is required with the pool or a builder: they fetch and upload bundles, results and runtime binaries through presigned URLs that point at store.objects.endpoint, and the NetworkPolicy must allow that egress. Set it to egress rules that reach the store, for example an in-cluster SeaweedFS: [{to: [{podSelector: {matchLabels: {app: seaweedfs}}}], ports: [{port: 8333}]}], or an external store: [{to: [{ipBlock: {cidr: 10.20.0.0/16}}], ports: [{port: 443}]}]. Or set networkPolicy.enabled to false." -}}
{{- end -}}
{{- end -}}

{{/* The egress rules every worker, pool and builder pod gets: the server, DNS and the object store. */}}
{{- define "linea.egressCommon" -}}
- to:
    - podSelector:
        matchLabels:
          {{- include "linea.serverSelector" . | nindent 10 }}
  ports:
    - port: http
- to:
    - namespaceSelector: {}
  ports:
    - {port: 53, protocol: UDP}
    - {port: 53, protocol: TCP}
{{- with .Values.networkPolicy.objectStorage }}
{{ toYaml . }}
{{- end }}
{{- if .Values.seaweedfs.enabled }}
- to:
    - podSelector:
        matchLabels:
          {{- include "linea.seaweedfsSelector" . | nindent 10 }}
  ports:
    - port: 8333
{{- end }}
{{- end -}}

{{/* The URL workers dial: the server's Service, or worker.serverURL. */}}
{{- define "linea.serverURL" -}}
{{- if .Values.worker.serverURL -}}
{{- .Values.worker.serverURL -}}
{{- else -}}
{{- printf "%s://%s.%s.svc:%v" (ternary "https" "http" (not (empty .Values.server.tls.secretName))) (include "linea.fullname" .) .Release.Namespace .Values.service.port -}}
{{- end -}}
{{- end -}}

{{/* The server config, composed from config and the chart's own settings. */}}
{{- define "linea.serverConfig" -}}
{{- $config := .Values.config -}}
{{- include "linea.checkTokens" . -}}
{{- if not (include "linea.acceptsWorkers" .) -}}
{{- fail "pool.enabled, worker.enabled or a worker token is required: a team server runs no Collector in its own process" -}}
{{- end -}}
{{- if not $config.bindings -}}
{{- fail "config.bindings is required: without a binding nothing is allowed" -}}
{{- end -}}
{{- $auth := deepCopy ($config.authentication | default dict) -}}
{{- if not (or $auth.oidc $auth.trusted_headers) -}}
{{- fail "config.authentication needs oidc or trusted_headers" -}}
{{- end -}}
{{- if $auth.oidc -}}
{{- if include "linea.hasOIDCSecret" . -}}
{{- $_ := set $auth.oidc "client_secret_file" "/etc/linea/oidc/client-secret" -}}
{{- end -}}
{{- end -}}
{{- $file := dict "version" 1 "authentication" $auth "bindings" $config.bindings -}}
{{- if include "linea.acceptsWorkers" . }}{{ $_ := set $file "worker_token_file" "/etc/linea/worker-token/token" }}{{ end -}}
{{- if include "linea.acceptsBuilds" . -}}
{{- $builds := dict "token_file" "/etc/linea/builder-token/token" -}}
{{- if .Values.builder.export.repository }}{{ $_ := set $builds "export" (dict "repository" .Values.builder.export.repository) }}{{ end -}}
{{- $_ := set $file "builds" $builds -}}
{{- end -}}
{{- include "linea.checkBundledStores" . -}}
{{- $o := include "linea.objects" . | fromYaml -}}
{{- if not (and $o.endpoint $o.bucket) -}}
{{- fail "store.objects needs endpoint and bucket" -}}
{{- end -}}
{{- $objects := dict "endpoint" $o.endpoint "bucket" $o.bucket "path_style" $o.pathStyle "access_key_file" "/etc/linea/store/access-key" "secret_key_file" "/etc/linea/store/secret-key" -}}
{{- if $o.prefix }}{{ $_ := set $objects "prefix" $o.prefix }}{{ end -}}
{{- if $o.region }}{{ $_ := set $objects "region" $o.region }}{{ end -}}
{{- $_ := set $file "store" (dict "database_url_file" "/etc/linea/store/database-url" "objects" $objects "storage_limit" $config.storageLimit "run_limit" $config.runLimit) -}}
{{- if $config.limits }}{{ $_ := set $file "limits" $config.limits }}{{ end -}}
{{- if $config.retention }}{{ $_ := set $file "retention" $config.retention }}{{ end -}}
{{- toYaml $file -}}
{{- end -}}

{{/* An image reference: repository:tag, with @digest when one is given. */}}
{{- define "linea.image" -}}
{{- .repository }}:{{ .tag }}{{ with .digest }}@{{ . }}{{ end -}}
{{- end -}}

{{/* A value from values, else the one kept in an existing Secret, else a new random one. Generated once per render: every template sees the same value. */}}
{{- define "linea.keptSecret" -}}
{{- $root := index . 0 -}}{{- $secret := index . 1 -}}{{- $key := index . 2 -}}{{- $given := index . 3 -}}
{{- $cache := $root.Values._generated | default dict -}}
{{- $id := printf "%s/%s" $secret $key -}}
{{- if not (hasKey $cache $id) -}}
{{- $value := $given -}}
{{- if not $value -}}
{{- $existing := lookup "v1" "Secret" $root.Release.Namespace $secret -}}
{{- if and $existing $existing.data (hasKey $existing.data $key) -}}
{{- $value = b64dec (index $existing.data $key) -}}
{{- else -}}
{{- $value = randAlphaNum 32 -}}
{{- end -}}
{{- end -}}
{{- $_ := set $cache $id $value -}}
{{- $_ := set $root.Values "_generated" $cache -}}
{{- end -}}
{{- index $cache $id -}}
{{- end -}}

{{- define "linea.postgresqlName" -}}
{{- printf "%s-postgresql" (include "linea.fullname" .) -}}
{{- end -}}

{{- define "linea.seaweedfsName" -}}
{{- printf "%s-seaweedfs" (include "linea.fullname" .) -}}
{{- end -}}

{{- define "linea.postgresqlPassword" -}}
{{- include "linea.keptSecret" (list . (include "linea.postgresqlName" .) "password" .Values.postgresql.password) -}}
{{- end -}}

{{- define "linea.seaweedfsAccessKey" -}}
{{- include "linea.keptSecret" (list . (include "linea.seaweedfsName" .) "access-key" .Values.seaweedfs.accessKey) -}}
{{- end -}}

{{- define "linea.seaweedfsSecretKey" -}}
{{- include "linea.keptSecret" (list . (include "linea.seaweedfsName" .) "secret-key" .Values.seaweedfs.secretKey) -}}
{{- end -}}

{{/* Fails on store values that conflict with the bundled stores. */}}
{{- define "linea.checkBundledStores" -}}
{{- if and (or .Values.postgresql.enabled .Values.seaweedfs.enabled) .Values.store.existingSecret -}}
{{- fail "store.existingSecret cannot be combined with postgresql.enabled or seaweedfs.enabled: the chart writes the store Secret for the bundled stores" -}}
{{- end -}}
{{- if and .Values.postgresql.enabled .Values.store.databaseURL -}}
{{- fail "store.databaseURL and postgresql.enabled are both set: use your PostgreSQL or the bundled one" -}}
{{- end -}}
{{- $o := .Values.store.objects -}}
{{- if and .Values.seaweedfs.enabled (or $o.endpoint $o.bucket $o.accessKey $o.secretKey) -}}
{{- fail "store.objects and seaweedfs.enabled are both set: use your object store or the bundled one" -}}
{{- end -}}
{{- end -}}

{{/* The store the server uses: the bundled stores, or store from values. */}}
{{- define "linea.databaseURL" -}}
{{- if .Values.postgresql.enabled -}}
{{- printf "postgres://linea:%s@%s:5432/linea?sslmode=disable" (include "linea.postgresqlPassword" .) (include "linea.postgresqlName" .) -}}
{{- else -}}
{{- .Values.store.databaseURL -}}
{{- end -}}
{{- end -}}

{{- define "linea.objects" -}}
{{- if .Values.seaweedfs.enabled -}}
{{- toYaml (dict "endpoint" (printf "http://%s:8333" (include "linea.seaweedfsName" .)) "bucket" .Values.seaweedfs.bucket "prefix" "" "region" "" "pathStyle" true "accessKey" (include "linea.seaweedfsAccessKey" .) "secretKey" (include "linea.seaweedfsSecretKey" .)) -}}
{{- else -}}
{{- toYaml .Values.store.objects -}}
{{- end -}}
{{- end -}}
