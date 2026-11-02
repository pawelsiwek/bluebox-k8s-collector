# Bluebox collector packaging

Values files that install the upstream
[`open-telemetry/opentelemetry-collector`](https://github.com/open-telemetry/opentelemetry-helm-charts/tree/main/charts/opentelemetry-collector)
chart for Bluebox's agent-free Kubernetes onboarding. Bluebox ships no
chart of its own. Paths below are relative to the repository root.

| | |
|---|---|
| Chart | `open-telemetry/opentelemetry-collector` **0.177.0** |
| Image (default) | `ghcr.io/pawelsiwek/bluebox-otelcol:0.1.0`, `command.name: bluebox-otelcol`. `main` holds a zero digest; tag `values-vX.Y.Z` pins the published digest (see "Image variants") |
| Image (switch) | `public.ecr.aws/dynatrace/dynatrace-otel-collector:0.58.1@sha256:29a09e6a…6c5589`, `command.name: dynatrace-otel-collector` (`overlays/dynatrace-image/<role>.yaml`) |
| Probe image | `curlimages/curl:8.23.0@sha256:11e64e28…6aa8ac` |
| Values release | `global.bluebox.collectorVersion` = `0.1.0` (exported as `bluebox.collector.version`) |

Each pinned image has one owner who bumps the digest and answers CVEs: the
collector packaging owner owns the collector, the curl probe and
kubernetes-mcp-server; the live-read owner owns the redactor and the
EdgeConnect pin.

## Layout

```
values/common.yaml            shared: image, hardening, token, exporter, heartbeat, allowlist
values/agent.yaml             release bluebox-otelcol-agent  (DaemonSet) + helm test probe
values/cluster.yaml           release bluebox-otelcol-cluster (Deployment)
values/pod-logs.agent.yaml    opt-in pod logs (agent only)
values/overlays/<name>/<role>.yaml   aks, eks, gke, autopilot, local, ai, live-read
values/values.schema.json     schema for global.bluebox (Bluebox knobs)
install/snippet.template.txt  install commands the wizard fills in
scripts/render.sh             helm template one release with a list of overlays
scripts/validate.sh           `<collector> validate` on a rendered config, in the image it deploys
scripts/collector_image.sh    image a rendered release runs (unpublished Bluebox image -> local build)
scripts/security_preview.py   RBAC, hostPaths, capabilities, PSA level, kubelet TLS, egress
tests/                        see "Tests"
```

**Layering (later wins).** `common` → `agent|cluster` → `pod-logs` (agent, optional)
→ distribution overlay (`aks` | `eks` | `gke` | `local`; Autopilot = `gke` then
`autopilot`) → `ai` → `live-read` (cluster). Helm merges maps but **replaces
lists**, so an overlay that changes a list (pipeline processors, k8s_attributes
labels, node conditions) restates it in full. `make validate` renders 20 real
combinations, not each overlay alone.

**Why `global.bluebox`.** The upstream chart schema rejects unknown top-level
keys. `global` is a free object, and `config`, `extraEnvs` and
`extraManifests` go through `tpl`, so the Bluebox knobs live there.
Helm does not apply `values.schema.json`: `make schema` does, and the wizard
validates its inputs against it. Template strings in values must not contain
spaces inside quoted literals. `toYaml` folds long lines at spaces, which
breaks the template.

## Releases and signals (contract C1)

| Release | Kind | Collects |
|---|---|---|
| `bluebox-otelcol-agent` | DaemonSet `bluebox-otelcol-agent`, Service `bluebox-otelcol` | `kubelet_stats` (node/pod/container/volume), app OTLP intake on 4317/4318, pod logs when enabled |
| `bluebox-otelcol-cluster` | Deployment, 2 replicas + PDB | `k8s_cluster` metrics, `k8s_events` (logs, `event.domain=k8s`), `k8s_objects` topology feed (pull, 120 s) |

- **Agent metadata** includes `container.image.name` and
  `container.image.tags`, the v1 name; v0 was `container.image.tag`.
- **Known v0 exception:** the `k8s_cluster` receiver still emits
  `container.image.tag` (v0). The upstream receiver has no switch to the v1
  name yet. Nothing in Bluebox reads the attribute, so the cluster release
  keeps it instead of adding a processor to drop it. Remove it once upstream
  emits only the v1 name.
- **Resource attributes on every signal:** `k8s.cluster.uid`,
  `k8s.cluster.name`, `bluebox.collector.version`, `bluebox.collector.role`,
  and v1 `k8s.*` names from `k8s_attributes`. The gates
  `processor.k8sattributes.EmitV1K8sConventions` and
  `DontEmitV0K8sConventions` are set explicitly on the command line.
- **Node cost labels** come from node labels, so there is no IMDS
  dependency. All distributions get `host.type`, `cloud.region` and
  `cloud.availability_zone`. The distro overlays add
  `k8s.node.label.capacity_type` and `k8s.node.label.node_pool`.
  `cloud.provider` is set per overlay.
- **`cloud.resource_id`** is best effort. It comes from
  `global.bluebox.cloudResourceId` when the wizard knows it. When empty it is
  omitted, not sent blank.
- **Cluster UID** defaults to a `lookup` of the kube-system namespace at
  install time. `helm template` has no cluster access (GitOps), so it must be
  passed there; the render fails with `global.bluebox.clusterUid_is_required`.
- **Exactly one active cluster collector.** `k8s_cluster`, `k8s_events` and
  `k8s_objects` share one `k8s_leader_elector`. The lease lives in the
  release namespace and is granted by a namespaced Role, not a ClusterRole
  rule.
- **Low-data metrics.** `filter/allowlist` drops every metric whose name is
  not matched by `global.bluebox.metrics.allow` / `extraAllow`. These are
  anchored RE2 patterns; write a literal dot as `[.]`. App OTLP is not
  filtered.
- **Heartbeat.** The collector's own telemetry, including
  `otelcol_process_uptime`, is exported every 30 s to a loopback OTLP
  receiver (`127.0.0.1:14318`). It then leaves through the same
  authenticated exporter as the data. A heartbeat therefore proves the
  export path, and it works with the file-based token.

## Topology feed redaction

`transform/redact` runs first in `logs/topology`, before anything is copied
out of the object:

1. On the map: delete `kubectl.kubernetes.io/last-applied-configuration`.
   Replace every annotation value that looks like JSON (`{…` or `[…`) with
   `"<redacted>"`, for pod and pod-template annotations. Escaped JSON is out
   of reach of step 2.
2. Serialize to JSON and rewrite with RE2:
   - every `"value"` scalar becomes `"<redacted>"`. This covers
     `env[].value` and `httpGet.httpHeaders[].value`. Toleration values are
     redacted too: an accepted over-redaction that fails closed.
   - every `"command"`/`"args"` array becomes `["<redacted>"]`.
   - any key containing
     `token|password|passwd|secret|apikey|api_key|api-key|credential|private_key|privatekey`
     (case-insensitive) with a scalar value is redacted.
3. The redacted string becomes both `k8s.object` and the log body. The raw
   object never leaves the processor. `error_mode: propagate` drops a batch
   rather than export it half-redacted.

`env[].valueFrom` references are objects, so they are kept.
`tests/redaction_test.sh` runs the rendered block verbatim over one
fixture for each of the 8 pulled kinds. It checks 21 planted secrets:
escaped quotes, `]` inside args, numeric values, init and ephemeral
containers, exec probes, workload templates, and JSON in an annotation. It
also requires a record per kind and no error in the collector log, because
a failing statement drops the whole batch.

The raw object map exists only between `transform/redact` and
`transform/finalize`, the last topology step, which replaces the body with
the redacted string. `dtk8stopology` sits between the two because it reads
the pod map. Its CONTAINER records copy only identity fields and
`k8s.container.image`, never env, command or args.

## Image variants

| | Bluebox image (default) | Dynatrace image (`overlays/dynatrace-image/<role>.yaml`) |
|---|---|---|
| Built from | Dynatrace distribution v0.58.1 manifest + `groupbyattrs` + Tobias Rauter's three components (`collector/`) | stock `dynatrace-otel-collector` 0.58.1 |
| K8S_CLUSTER record | `dtk8stopology`, once per pull | second `k8s_objects` pull of the kube-system Namespace, turned into the cluster record by OTTL (`logs/cluster`) |
| Pod workload | `groupbyattrs/pod` + `k8s_attributes` | `ownerReferences` via OTTL (ReplicaSet → Deployment by `pod-template-hash`) |
| CONTAINER entities with image | `dtk8stopology` fan-out | none |
| OOM-kill inferred events, event throttle | `dtk8seventsprocessor` over a pod MODIFIED watch; the watch records are dropped by `filter/no_raw_objects` and never exported | none |
| Threshold alerts | `alertingengine`, opt-in with `overlays/alerting/cluster.yaml` (off by default: Davis alerts on the metrics) | none |

The Bluebox image is a bridge. Switch to the Dynatrace image once Dynatrace
ships equivalent components or native topology.

On `main`, `image.digest` holds a zero placeholder (`make static` reports
it). The release workflow pins the published digest on tag `values-vX.Y.Z`.
On the zero digest, `scripts/collector_image.sh` runs `BLUEBOX_IMAGE`,
default `localhost/bluebox-otelcol:dev`, built with
`make -C collector image`. The tests need that image for the default variant.

## Token

The token Secret `bluebox-otelcol-token` (key `token`) is mounted as a
directory (never `subPath`). `bearertokenauth` reads it with
`wait_for_token_file` and a 5-minute retry budget, and prepends
`global.bluebox.tokenScheme`: `Bearer` for platform tokens, `Api-Token` for
classic tokens. The file holds the bare token; only its first
whitespace-delimited word is used, so a trailing newline is harmless.

**Rotation without restart:** `kubectl apply` the Secret with a new value.
kubelet swaps the `..data` symlink (up to its sync period, about 1–2 min).
The fsnotify watch fires when the old generation is deleted, and the next
export uses the new token.

## Hardened defaults

- Non-root `65532`, gid `0`, `runAsNonRoot`, no privilege escalation, drop
  `ALL`, `RuntimeDefault` seccomp, read-only root filesystem.
- `memory_limiter` first in every pipeline, `GOMEMLIMIT` set from the
  memory limit, PDB on the cluster release.
- PriorityClass `bluebox-otelcol-agent`, created by the agent release.
- No hostPort (the OTLP intake is a Service with
  `internalTrafficPolicy: Local`). No `nodes/proxy`: no cAdvisor scrape,
  and kubeletstats needs only `nodes/stats`. No Secret or ConfigMap RBAC.
- Proxy: `global.bluebox.proxy.httpsProxy` / `noProxy`, which keeps `.svc`
  direct.
- Extra CA: `global.bluebox.caBundle=true` plus ConfigMap
  `bluebox-otelcol-ca` (key `ca.crt`).
- Registry mirror: `global.imageRegistry` plus `imagePullSecrets`.
- **PSA:** without pod logs, both releases run in a namespace enforcing
  `restricted`. `make static` asserts this on every render; the kind e2e
  asserts it with real admission.

## Pod logs (opt-in) and the log loss window

`pod-logs.agent.yaml` reads `/var/log/pods/{<allowlist>}_*/*/*.log`,
read-only. kube-system and the collector namespace are always excluded,
even when `"*"` is listed. It needs:

- **uid 0.** kubelet writes pod logs `0640 root:root`. Capabilities stay
  dropped and the root filesystem stays read-only.
- **PSA `privileged` on the namespace,** because hostPath is not allowed by
  `baseline`. The snippet sets that label only on this path, and the
  security preview says so.

**Log loss window.** Offsets are not persisted. Persisting them would need
a writable checkpoint hostPath, which Autopilot forbids. After any agent
restart (rollout, eviction, OOM, node drain), reading resumes at the end of
each file (`start_at: end`). Lines written while the agent was down are
lost. Expect a gap of roughly the restart duration, typically 5–30 s per
node during a rollout.

## Drain gap

- **Cluster release:** the leader holds the lease. On eviction, the standby
  replica acquires it after the lease expires, about 15 s with the default
  elector timings. Events emitted in that window are not re-listed, because
  `k8s_events` starts from "now". Topology and `k8s_cluster` catch up on
  their next pull. The `local` overlay runs one replica without a PDB, so
  its gap lasts until the pod is rescheduled.
- **Agent:** a draining node's agent stops with the node. Its exporter queue
  (in memory, up to 1000 batches) is flushed within
  `terminationGracePeriodSeconds: 30`; anything left is lost. App OTLP sent
  to the node-local Service during the drain fails over to nothing, because
  of `internalTrafficPolicy: Local`. Apps should retry.

## Kubelet TLS

`insecure_skip_verify: true` is the base default. EKS and kind kubelet
serving certificates are not signed by the cluster CA, and verified mode on
AKS and GKE is not yet proven on real clusters. The security preview prints
the mode. To verify, set `config.receivers.kubelet_stats.insecure_skip_verify=false`
and `ca_file: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt`.

## Distribution notes

- **AKS, EKS, GKE:** node cost labels and `cloud.provider` only.
- **GKE Autopilot** (`autopilot`, self-contained; `make static` checks it renders the same as `gke` + `autopilot`):
  - explicit requests equal to limits;
  - only the Spot, compute-class and arm64 tolerations, no blanket
    `Exists`;
  - pod logs use `/var/log/pods` read-only, with no checkpoint directory;
  - no `nodes/proxy`.

  Autopilot's partner allowlist does not cover the OTel collector, so uid 0
  for pod logs is the part most likely to be rejected; check it with
  `kubectl apply --dry-run=server`.
- **Local** (kind, minikube, Docker Desktop): low RAM, arm64, one cluster
  replica, logs on (with `pod-logs.agent.yaml`, `namespaces: ["*"]`).
- **AI** (opt-in when the verify job detects GPU or inference metrics):
  - per-node Prometheus scrape of DCGM (`app=nvidia-dcgm-exporter:9400`),
    AMD (`app.kubernetes.io/name=metrics-exporter:5000`) and pods annotated
    `prometheus.io/scrape=true` (vLLM, Triton, EPP);
  - an allowlist on both sides;
  - `vllm:gpu_cache_usage_perc` renamed to `vllm:kv_cache_usage_perc`;
  - `pod_uid` dropped;
  - `AcceleratedHardwareReady` added to the reported node conditions.

## Live read (`live-read` overlay, cluster release, experiment)

The flow is EdgeConnect → `k8s-mcp-<uid8>.bluebox.internal:8080` → redactor
→ `kubernetes-mcp-server` → API server (C2).

- **Hostname.** EdgeConnect has no host-mapping setting. The C2 hostname
  resolves through a pod `hostAliases` entry to the redactor sidecar on
  `127.0.0.1:8080`, which forwards to the `bluebox-k8s-mcp` Service.
- **kubernetes-mcp-server:**
  - `read_only`, `disable_destructive`, `toolsets=["core"]`;
  - Secret and ConfigMap are both in `denied_resources` and absent from
    RBAC;
  - get/list only;
  - a NetworkPolicy admits only the EdgeConnect pods.
- **EdgeConnect:** 2 replicas plus a PDB. Egress is 443, DNS, and the MCP
  server only. The OAuth client comes from Secret `bluebox-edgeconnect`
  (`client-id`, `client-secret`).
- **Size cap.** The 64 KiB `result_too_large` cap from C2 is enforced by the
  backend on the `dt-adhoc-relay` path, not here. `max_result_bytes`
  (1 MiB) only bounds the server.
- **Unverified until the live-read spike runs:**
  - EdgeConnect configured from environment variables only, as a non-root
    user;
  - the redactor's flags;
  - the redactor image digest, a zero-digest placeholder that `make static`
    reports.

## Install

`install/snippet.template.txt` is what the wizard fills in. It is plain
`helm`/`kubectl` lines with no variables, pipes or continuations, so it
works in bash, zsh, fish, PowerShell and cmd. The token Secret is pasted on
stdin (`kubectl apply -f -`), so it never appears on a command line or in
history. Both releases install with `--wait --atomic`. The `helm test` probe
belongs to the agent release, which is installed first. Verify with:

```
helm test bluebox-otelcol-agent --namespace bluebox-otelcol --logs
```

That prints the C4 keys (`otlp_status`, `cause`, `ready`, `cluster_uid`,
`nodes`, `namespaces`, `pods`, `collector_version`) and exits 0 only when
`ready=true`. It is a Pod hook, because `helm test --logs` collects only
Pod logs.

**Files the snippet references** (relative to `values/`, stable names):

| Role | Always | Distribution (one) | Optional, in this order |
|---|---|---|---|
| agent | `common.yaml`, `agent.yaml` | `overlays/{aks,eks,gke,autopilot,local}/agent.yaml` (`autopilot` is self-contained; it also works after `gke`) | `pod-logs.agent.yaml`, `overlays/dynatrace-image/agent.yaml`, `overlays/ai/agent.yaml`, `overlays/live-read/agent.yaml` (empty) |
| cluster | `common.yaml`, `cluster.yaml` | `overlays/{aks,eks,gke,autopilot,local}/cluster.yaml` (same) | `overlays/dynatrace-image/cluster.yaml`, `overlays/ai/cluster.yaml`, `overlays/alerting/cluster.yaml` (Bluebox image only), `overlays/live-read/cluster.yaml` |

Every distribution and optional overlay has a file for both roles, except
`alerting` (cluster only) and `pod-logs` (agent only).

**Uninstall:**

```
helm uninstall bluebox-otelcol-cluster -n bluebox-otelcol
helm uninstall bluebox-otelcol-agent -n bluebox-otelcol
kubectl delete namespace bluebox-otelcol
```

Deleting the namespace also removes the Secret. The PriorityClass goes with
the agent release.

## GitOps notes

- `helm template` / Argo CD cannot run `lookup`. Set
  `global.bluebox.clusterUid` explicitly (the kube-system namespace UID).
- Keep the token out of Git:
  - create `bluebox-otelcol-token` with External Secrets or Sealed Secrets;
  - sync it in an earlier wave than the releases, because the pods block on
    the token file.
- Argo CD does not run `helm.sh/hook: test`. Run the probe by hand, or
  apply the rendered Pod as a PostSync hook.
- Namespace PSA labels belong in the namespace manifest you manage:
  `restricted`, or `privileged` only with pod logs.
- Pin the values by the commit a `values-vX.Y.Z` tag points to: `-f https://raw.githubusercontent.com/pawelsiwek/bluebox-k8s-collector/<commit>/values/…`,
  or vendor the files. A tag can move; a commit cannot. The Bluebox snippet uses the commit.

## Security preview

```
make render && uv run --no-project --with pyyaml python scripts/security_preview.py .out/agent_eks.yaml .out/cluster_eks.yaml [--json]
```

This prints, per release:

- the namespace PSA level needed;
- kubelet TLS mode;
- egress `host:port`;
- per workload: run-as user, added capabilities and hostPaths (read-only or
  not);
- every RBAC rule with its scope.

## Tests

`make test-values` runs everything for both image variants. It needs the
Bluebox image locally (`BLUEBOX_IMAGE`, default `localhost/bluebox-otelcol:dev`,
from `make -C collector image`); the Dynatrace image is pulled by digest.

| Target | What it proves | Runs |
|---|---|---|
| `make validate` | `helm template` of every layering in `COMBOS` and `validate` of each config in the image it deploys (pinned image, fake in-cluster SA) | container |
| `make allowlist` | every allowlist entry matches a metric kubeletstats / k8s_cluster v0.163.0 emit with this config | local |
| `make podlogs` | the `{a,b}` namespace glob reads only allowlisted namespaces; kube-system and the collector namespace stay excluded with `*` | container |
| `make static` | PSA `restricted` without pod logs, no hostPort, no `nodes/proxy`, no Secret/ConfigMap RBAC, digest pins, v1 gates, one leader elector, redaction first, intake Service `Local` | local |
| `make schema` | every values file matches `values/values.schema.json`; 10 bad inputs rejected | local (uv) |
| `make snippet` | snippet is shell-agnostic, Secret via stdin, `--wait --atomic`, files exist | local |
| `make redaction` | both image variants: no env value, command argument or probe header reaches the exporter; `valueFrom` kept; K8S_CLUSTER record; CONTAINER fan-out with image (Bluebox) or ownerReferences workload (Dynatrace) | container |
| `make events` | Bluebox image: events forwarded with `event.domain=k8s`, OOM kill inferred from a pod watch, raw pod watch records never exported | container |
| `make heartbeat` | `otelcol_process_uptime` with C1 attributes; token A → B without restart (kubelet-style projection) | container |
| `make probe` | C4 key set and order; ok/401/403/dns; token never printed | container |
| `make e2e-kind` | real PSA admission, live redaction, v1 names, event dedup with 2 replicas, rotation, `helm test` | CI (`ci.yml`, job `kind-e2e`) |
