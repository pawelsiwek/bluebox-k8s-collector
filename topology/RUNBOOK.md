# Bluebox K8s topology: tenant setup runbook (operators, provisioned tenants only)

This runbook installs native `K8S_*` topology for Bluebox's agent-free Kubernetes onboarding on one Dynatrace tenant. It has three parts:
- the CA certificate;
- the Bluebox-signed extension `custom:dt-k8s-otel-topology`;
- one logs-routing entry, merged into the tenant's routing singleton without touching anyone else's entries.

It is the manual bridge until Bluebox's governed config-write path can do it. Run it **only on tenants Bluebox provisioned**: a bring-your-own tenant must not be asked to trust the Bluebox CA without explicit customer consent.

| Artifact | Path |
|---|---|
| Extension source, upstream 1.3.3 pinned byte-for-byte | `topology/extension/src/`, pin in `extension/UPSTREAM.json` |
| Routing entry template (with the per-workspace marker) | `topology/routing/logs-routing-entry.json` |
| Merge tool | `topology/routing/merge_routing.py` (Python 3.9+, stdlib; reads YAML exports if PyYAML is installed) |

## 0. Preconditions — all required, stop if any fails

1. **Skill gating fix is live.** The Bluebox change where `dt-obs-otel-kubernetes` is no longer hidden by `K8S_TOPOLOGY` is deployed in the Bluebox environment this tenant is connected to. Without it, the native entities this runbook creates switch the agent onto the wrong Kubernetes skill.
2. **No DynaKube on the tenant.** This query must return 0. If it doesn't, stop: the extension's entity IDs would collide with the ActiveGate model.
   ```bash
   dtctl query 'fetch metric.series, from:now()-1h | filter startsWith(metric.key, "dt.kubernetes.") | summarize count()' --context $CTX
   ```
3. **Smartscape extraction from logs is enabled** on the tenant (it is disabled on some tenants). If it is disabled, report "topology unavailable" and stop.
4. **Bluebox collectors stamp the routing attributes.** Topology and event logs carry `event.provider` `KUBERNETES_OTEL_TOPO` or `KUBERNETES_EVENT`, and the C1 resource attribute `bluebox.collector.role`. Both are set by the Bluebox collector values. The entry requires `bluebox.collector.role`, so logs from other agents (DynaKube, other teams) are never claimed. It also accepts `dt.openpipeline.source == "custom:dt-k8s-otel-topology"`, because the collector notes say that value may be reassigned server-side for logs.
5. **You have:**
   - a `dtctl` context `$CTX` for the tenant, with settings read/write and extension install rights;
   - the Bluebox signing material (§1);
   - the workspace UUID `$WS`.

```bash
export CTX=<dtctl-context> WS=<workspace-uuid> T=topology
export RUN=$(mktemp -d)   # keep this directory until the run is verified
```

## 1. Get the signed extension from a release

The `topology-extension` job of the release workflow assembles the pinned upstream source, signs it with the Bluebox developer certificate and checks the signature against `topology/ca.pem`. It attaches `dt-k8s-otel-topology-<version>.zip`, its `.sha256` and `ca.pem` to the GitHub Release. Nobody signs on a laptop.

```bash
export TAG=<release tag, e.g. v0.1.2> V=1.3.3
gh release download $TAG --repo pawelsiwek/bluebox-k8s-collector --dir $RUN \
  --pattern "dt-k8s-otel-topology-$V.zip*" --pattern ca.pem
(cd $RUN && shasum -a 256 -c dt-k8s-otel-topology-$V.zip.sha256)
cmp $RUN/ca.pem $T/ca.pem && echo "CA matches the repository"
```

**Key custody.**
- `topology/ca.pem` is the public Bluebox extension CA. Its key stays offline with the named key custodian and never enters the repository or CI.
- The developer certificate lives only in the `EXTENSION_SIGNING_PEM` secret of the `release` environment. Required reviewers on that environment make signing a two-person step.
- The POC CA generated on 2026-10-07 is throwaway and must never sign anything that reaches a tenant.
- A CA key compromise reaches every tenant that trusts the CA. Revocation means removing the CA from every tenant (§7) and re-signing with a new CA.

## 2. Upload the CA certificate

Upload the Bluebox CA public certificate (`$RUN/ca.pem`, **never** the key) as a trusted extension root on the tenant. Post it to `/api/v2/extensions/developerCertificates` with a classic token that has the extension write scope. The governed path may need credential-vault write instead. Record which route the tenant accepted. Upload it once per tenant: re-uploading the same CA is harmless.

## 3. Install the extension

```bash
dt extensions upload $RUN/dt-k8s-otel-topology-$V.zip --tenant-url https://<tenant>.live.dynatrace.com --api-token "$DT_API_TOKEN"
```

Then resolve the objectId of the bundled logs pipeline. The routing entry references it by objectId, not by `customId`, and it changes if the extension is reinstalled.

```bash
export PIPELINE_ID=$(dtctl get settings --schema builtin:openpipeline.logs.pipelines -o json --no-agent --context $CTX \
  | jq -r '.[] | select(.value.customId=="k8s-otel-logs") | .objectId // .objectid')
test -n "$PIPELINE_ID" && echo ok
```

## 4. Merge the logs-routing entry safely

The logs routing object is a per-tenant singleton, and `dtctl apply` replaces its whole `routingEntries` list. The merge tool therefore never writes to the tenant. It changes only the entry carrying `[bluebox:k8s-topology workspace=$WS]` and inserts it first, because routing is first-match-wins. Foreign entries keep their content and order. If an entry with our marker already exists with different content, the tool refuses with exit code 3: someone edited it, so a human decides.

```bash
# 4.1 read
dtctl get settings --schema builtin:openpipeline.logs.routing -o json --no-agent --context $CTX > $RUN/before.json
# An empty list ([]) means the tenant has no routing object yet; then use
#   echo '{"schemaId":"builtin:openpipeline.logs.routing","scope":"environment","value":{"routingEntries":[]}}' > $RUN/before.json

# 4.2 merge (prints {"action": "inserted"|"unchanged", "input_sha256": …, "foreign_entries": n})
python3 $T/routing/merge_routing.py merge --export $RUN/before.json --workspace-id $WS \
  --pipeline-id "$PIPELINE_ID" --out $RUN/merged.json 2> $RUN/merge.out; cat $RUN/merge.out
```

If `action` is `unchanged`, the tenant already has the entry. Skip to §5: re-running the runbook is idempotent.

```bash
# 4.3 the tenant must not have changed since the read
python3 $T/routing/merge_routing.py check-unchanged --export <(dtctl get settings --schema builtin:openpipeline.logs.routing -o json --no-agent --context $CTX) \
  --sha256 "$(jq -r .input_sha256 $RUN/merge.out)"

# 4.4 review: the only difference must be our one added entry, at the top.
#     dtctl diff has no file-vs-remote mode for settings (v0.34), so diff two files.
python3 $T/routing/merge_routing.py normalize --export $RUN/before.json --out $RUN/before.norm.json
dtctl diff -f $RUN/before.norm.json -f $RUN/merged.json
dtctl apply -f $RUN/merged.json --dry-run --context $CTX   # expect: updated (or created on an empty tenant)

# 4.5 apply, then 4.6 re-read and verify foreign entries are byte-identical and in order, ours first
dtctl apply -f $RUN/merged.json --context $CTX
dtctl get settings --schema builtin:openpipeline.logs.routing -o json --no-agent --context $CTX > $RUN/after.json
python3 $T/routing/merge_routing.py verify --before $RUN/before.json --after $RUN/after.json \
  --workspace-id $WS --pipeline-id "$PIPELINE_ID" && echo verified
```

If 4.3 or 4.6 fails, start again from 4.1. Never restore `before.json` with `apply` to undo: that would discard anyone else's concurrent change. Use `remove` (§7) instead.

### Optimistic concurrency: what the API offers

This was investigated offline in the `dtctl` source (v0.34) and its SDK (v0.40), which Bluebox's backend vendors:
- `dtctl` reads and writes settings through `/platform/classic/environment-api/v2/settings/objects`.
- It requests only `objectId,scope,schemaId,schemaVersion,externalId,summary,value,modificationInfo`, so it never sees an `updateToken`.
- On `apply` it GETs the object and PUTs with `If-Match: <schemaVersion>`. That guards against schema-version drift, not against a concurrent edit of the entries.
- So `dtctl apply` alone is last-writer-wins. The `check-unchanged` step (4.3) narrows the race window to seconds, and `verify` (4.6) detects a lost update after the fact.

The classic Settings 2.0 API documents an `updateToken` field: when it is sent on PUT, the update is rejected if the object changed since the read. Confirm it on the tenant, ideally once, and record the result on the Bluebox topology tracking issue:

```bash
OBJ=$(jq -r '.[0].objectId // .[0].objectid' $RUN/before.json)
curl -s -H "Authorization: Bearer $DT_PLATFORM_TOKEN" \
  "https://<tenant>.apps.dynatrace.com/platform/classic/environment-api/v2/settings/objects/$OBJ?fields=objectId,schemaVersion,updateToken" | jq
```

If `updateToken` is present, replace 4.5 with a conditional write. Take the token from the same GET that produced the merge input, so the check and the write are one atomic step:

```bash
curl -s -H "Authorization: Bearer $DT_PLATFORM_TOKEN" \
  "https://<tenant>.apps.dynatrace.com/platform/classic/environment-api/v2/settings/objects/$OBJ?fields=objectId,schemaId,scope,value,updateToken" > $RUN/before.json
# … 4.2 merge with --export $RUN/before.json …
jq --arg t "$(jq -r .updateToken $RUN/before.json)" '{value: .value, updateToken: $t}' $RUN/merged.json > $RUN/put.json
curl -s -X PUT -H "Authorization: Bearer $DT_PLATFORM_TOKEN" -H "Content-Type: application/json" \
  --data @$RUN/put.json "https://<tenant>.apps.dynatrace.com/platform/classic/environment-api/v2/settings/objects/$OBJ"
```

A conflict response means someone changed routing in between: start again from 4.1. The automated config-write path should use `updateToken` if it exists.

## 5. Verify topology

After the Bluebox collector has been running for 2–3 minutes:

```bash
dtctl query 'fetch logs, from:now()-10m | filter dt.openpipeline.source == "custom:dt-k8s-otel-topology" | summarize count(), by:{dt.openpipeline.pipelines}' --context $CTX
dtctl wait query 'smartscapeNodes K8S_CLUSTER | filter k8s.cluster.uid == "<kube-system UID>"' --for=count=1 --timeout 10m --context $CTX
dtctl query 'smartscapeNodes "K8S_*" | summarize count(), by:{type}' --context $CTX
```

- **Pass:** the first query shows `k8s-otel-logs`, and `K8S_CLUSTER`, `K8S_NODE`, `K8S_POD` and workload nodes exist.
- **Logs in `logs:default`:** the entry is not matching. Check that it is first and that the `pipelineId` is right.
- **Pipeline ran but no nodes:** check whether Smartscape extraction is disabled (precondition 3).

Payloads arrive redacted per C1 from the collector, so do a spot check: a `fetch logs` on a topology record must show `<redacted>` for env values, and no `last-applied-configuration`.

## 6. Re-running

The whole runbook is safe to repeat:
- the CA upload and extension install are idempotent for the same version;
- `merge` reports `unchanged` once the entry is present.

To move to a new pinned extension version, repeat §1 and §3. If the pipeline objectId changed, `merge` refuses with exit code 3, because our entry now differs. Run `remove` with the **old** pipeline id, then `merge` with the new one.

## 7. Removal — only when the workspace's last cluster is removed

Topology is shared by every cluster of the workspace, so removing it while one cluster remains breaks that cluster's entities. Confirm in Bluebox that the workspace has no remaining Kubernetes cluster resources, then:

```bash
dtctl get settings --schema builtin:openpipeline.logs.routing -o json --no-agent --context $CTX > $RUN/before.json
python3 $T/routing/merge_routing.py remove --export $RUN/before.json --workspace-id $WS --pipeline-id "$PIPELINE_ID" --out $RUN/removed.json
python3 $T/routing/merge_routing.py normalize --export $RUN/before.json --out $RUN/before.norm.json
dtctl diff -f $RUN/before.norm.json -f $RUN/removed.json && dtctl apply -f $RUN/removed.json --context $CTX
dtctl get settings --schema builtin:openpipeline.logs.routing -o json --no-agent --context $CTX > $RUN/after.json
python3 $T/routing/merge_routing.py verify --before $RUN/before.json --after $RUN/after.json --workspace-id $WS --pipeline-id "$PIPELINE_ID" --expect-absent
```

- Uninstall the extension only if no other `[bluebox:k8s-topology workspace=…]` entry remains on the tenant.
- Remove the CA only if no other Bluebox-signed extension remains installed.

## Tooling tests

```bash
(cd topology/routing && python3 -m unittest -v test_merge_routing)
(cd topology && python3 -m unittest -v test_extension_pin)
```

The merge tests run against a recorded, scrubbed routing export with foreign entries (`routing/testdata/`).
