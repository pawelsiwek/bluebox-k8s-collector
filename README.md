# bluebox-k8s-collector

Everything that runs in a customer's Kubernetes cluster for Bluebox's
agent-free Kubernetes monitoring. These are bridge artifacts: they exist
until Dynatrace's own collector image ships equivalent components (see
"Exit plan").

The repository publishes:

- images to GHCR: `ghcr.io/pawelsiwek/bluebox-otelcol` (collector) and
  `ghcr.io/pawelsiwek/k8s-mcp-redactor` (live-read redactor);
- Helm values files for the upstream
  [`open-telemetry/opentelemetry-collector`](https://github.com/open-telemetry/opentelemetry-helm-charts/tree/main/charts/opentelemetry-collector)
  chart, consumed by raw URL at a git tag.

The Bluebox backend renders the install snippet. Bluebox ships no Helm chart
of its own.

## Layout

| Path | What |
|---|---|
| `collector/` | OCB build of the Dynatrace OTel Collector distribution v0.58.1 plus three Kubernetes topology components. See [`collector/README.md`](collector/README.md). |
| `redactor/` | `k8s-mcp-redactor`, the read-only redacting proxy in front of `kubernetes-mcp-server` for live read. See [`redactor/README.md`](redactor/README.md). |
| `values/` | Values tree: `common.yaml`, `agent.yaml`, `cluster.yaml`, `pod-logs.agent.yaml`, `overlays/<name>/<role>.yaml`, `values.schema.json`. See [`docs/values.md`](docs/values.md). |
| `install/` | `snippet.template.txt`, the install commands the Bluebox wizard fills in. |
| `scripts/` | Render, validate, security preview; `scripts/ci/` holds the Trivy gate and the release pin script. |
| `tests/` | Values tests (render, validate, static policy, schema, snippet, redaction, events, heartbeat, probe) and the kind e2e. |
| `topology/` | Tenant-side topology artifacts: pinned extension, logs-routing entry, `merge_routing.py`, [`RUNBOOK.md`](topology/RUNBOOK.md). |

## Install

The Bluebox backend renders this shape (template: `install/snippet.template.txt`):

1. Create namespace `bluebox-otelcol` with the PSA label (`restricted`, or
   `privileged` only when pod logs are on).
2. Apply Secret `bluebox-otelcol-token` with key `token` from stdin.
3. Install release `bluebox-otelcol-agent` (DaemonSet), then
   `bluebox-otelcol-cluster` (Deployment). Both use
   `open-telemetry/opentelemetry-collector` with `--wait --atomic`.
4. Run `helm test bluebox-otelcol-agent --namespace bluebox-otelcol --logs`.

Each release layers values files by URL, later files win:

```
BASE=https://raw.githubusercontent.com/pawelsiwek/bluebox-k8s-collector/<tag>/values
-f $BASE/common.yaml -f $BASE/<role>.yaml [-f $BASE/pod-logs.agent.yaml] \
  -f $BASE/overlays/<distro>/<role>.yaml [-f $BASE/overlays/<opt>/<role>.yaml ...] \
  --set global.bluebox.clusterName=<name> --set global.bluebox.endpoint=<otlp-url>
```

Bluebox knobs live under `global.bluebox.*` (schema:
`values/values.schema.json`). Use a `values-vX.Y.Z` tag for `<tag>`, because
only those tags pin real image digests (see "Releasing"). The file list per
role, layering rules, GitOps notes and uninstall steps are in
[`docs/values.md`](docs/values.md) § "Install".

## Development

```
make test-go         # collector components + redactor: vet, race tests, gofmt
make test-topology   # extension pin + routing merge tool
make image           # local collector image (localhost/bluebox-otelcol:dev); needs > 4 GiB RAM
make test-values     # values suite; needs the local collector image, helm, yq, uv, podman or docker
make e2e-kind        # kind e2e
```

CI (`.github/workflows/ci.yml`) runs all of these on every push and pull
request. It builds the amd64 collector image once and runs the values tests
and the kind e2e against it.

## Releasing

1. Push tag `vX.Y.Z` on `main`.
2. `release.yml` runs CI, then for each image:
   - builds amd64 and runs the Trivy gate before any push;
   - pushes `linux/amd64` + `linux/arm64` as `ghcr.io/pawelsiwek/<image>:X.Y.Z`
     with a BuildKit SBOM attestation;
   - signs build provenance with `actions/attest-build-provenance`.
3. It then pins `values/` to the pushed digests (`scripts/ci/pin-release.sh`),
   commits that on top of `vX.Y.Z`, and pushes tag `values-vX.Y.Z`. Point
   Bluebox at that tag.
4. It creates the GitHub Release `vX.Y.Z`, with the image digests and SPDX
   SBOM files.

Values on `main` keep zero placeholder digests. `make static` reports them;
`make pinned` fails on them.

After the first release, make both GHCR packages public. Otherwise every
cluster needs an `imagePullSecret`.

Verify an image:
`gh attestation verify oci://ghcr.io/pawelsiwek/bluebox-otelcol:X.Y.Z --repo pawelsiwek/bluebox-k8s-collector`.

`vuln-scan.yml` runs govulncheck and the Trivy gate on the latest release
daily. Renovate (`renovate.json`) bumps Go modules, Dockerfiles and actions;
it leaves the OpenTelemetry Collector pins and the third-party image digests
in `values/` to their owners.

## Exit plan

The collector image exists only because the stock Dynatrace image lacks
CONTAINER entities with image, OOM-kill inferred events and threshold alerts.
When Dynatrace ships equivalent components or native topology:

1. Make `values/overlays/dynatrace-image/<role>.yaml` the default in the
   Bluebox snippet. It already runs the stock
   `public.ecr.aws/dynatrace/dynatrace-otel-collector` image, and CI tests it.
2. Stop publishing `bluebox-otelcol` and remove `collector/`.

The values tree, redactor and topology artifacts stay here until Bluebox
moves them to a Dynatrace-owned location.

## License

Apache-2.0 ([`LICENSE`](LICENSE)). Attribution: [`NOTICE`](NOTICE) and
[`collector/NOTICE`](collector/NOTICE).
