# Bluebox Kubernetes collector (`collector/`)

The collector image for Bluebox's agent-free Kubernetes onboarding.
`ghcr.io/pawelsiwek/bluebox-otelcol` is the
[Dynatrace OpenTelemetry Collector distribution](https://github.com/Dynatrace/dynatrace-otel-collector)
plus three Kubernetes topology components from Tobias Rauter's
`dt-otel-k8s-platform-monitoring`. Those components restore three things
the stock Dynatrace image lacks: CONTAINER entities with
`k8s.container.image`, OOM-kill inferred events and event throttling, and
threshold alerts.

**It is a bridge.** Switch the values back to the Dynatrace image once Dynatrace ships equivalent
components or native Kubernetes topology. The values files live in this
repository's `values/`, which also holds the stock-image variant
(`values/overlays/dynatrace-image/<role>.yaml`): it keeps everything except
the three components.

## What is in the binary

| Source | What | Pinned to |
|---|---|---|
| `build/dynatrace-manifest-v0.58.1.yaml` | Dynatrace distribution manifest, vendored verbatim | tag `v0.58.1` (commit `5920bfa6`), OpenTelemetry Collector `v0.162.0` / `v1.68.0` |
| `builder-config.yaml` | OCB config derived from that manifest | see below |
| `processor/dtk8stopology` | fans one K8S_POD topology record out to N CONTAINER records with `k8s.container.image` | vendored from `trauter/dt-otel-k8s-platform-monitoring@72bed8b` |
| `processor/dtk8seventsprocessor` | per-entity / per-cluster event throttle; OOM-kill → `KUBERNETES_INFERRED_EVENT` | same |
| `connector/alertingengine` | metric thresholds → `KUBERNETES_ANOMALY_DETECTION` events (off by default in the values: Davis alerts instead) | same |

`builder-config.yaml` is the Dynatrace manifest with exactly four changes:

- it drops the Dynatrace-internal `eecprovider` confmap provider, which
  lives in their repository;
- it adds `groupbyattrsprocessor` at the manifest's contrib version, for the
  topology namespace-batch fix;
- it adds the three components above;
- it adds security overrides (`replaces:`), described below.

`build/manifest_test.go` fails when the config drifts from the vendored
manifest. It also fails when the module's `go.opentelemetry.io/collector/*`
pins stop matching the distribution's release; otherwise OCB's module
resolution would silently upgrade the whole collector.

### Security overrides

The pinned distribution's `golang.org/x/net` v0.59.0 has fixable HIGH
findings: Trivy reports CVE-2026-78669, and govulncheck reports
GO-2026-6603, -6610, -6611, -6612 and -6617 as reachable through the
`http2` paths of the OTLP receiver and zpages. `replaces:` forces
`golang.org/x/net` v0.60.0. Remove each override once the pinned Dynatrace
release carries the fix; the image-scan gate and the daily govulncheck leg
show when an override is needed.

Licensing: Apache-2.0 (`LICENSE`). Attribution and the list of
modifications are in `NOTICE`, and both files ship in the image under
`/licenses/`.

## Build and test

```
cd collector
make test                                 # vet, race tests, gofmt (CI: ci.yml job `collector`)
make image                                # podman/docker build for the local platform
make validate CONFIG=<rendered relay.yaml>  # otelcol validate with the built image
make govulncheck                          # generate the distribution and scan the whole binary
```

Compiling the whole distribution needs several GiB of RAM. One AWS SDK
package alone was OOM-killed in a 4 GiB podman VM, even with
`GO_BUILD_FLAGS=-p=2`. On a small VM, give the VM more memory, or
cross-compile natively: run `make generate`, then in `_build` run
`CGO_ENABLED=0 GOOS=linux GOARCH=<arch> go build`, and copy the binary into
the final stage.

Every Go command runs with `GOWORK=off`. The collector dependency graph is
large and pinned to the Dynatrace distribution's release.

## Image

`Dockerfile`:

- **Builder stage:** `golang:1.26.10-bookworm` (digest-pinned), running
  natively on `$BUILDPLATFORM`. It runs OCB `v0.162.0` with
  `--skip-compilation`, then `go build -trimpath -buildvcs=false` with
  `CGO_ENABLED=0` for `$TARGETARCH`.
- **Final stage:** `distroless/static-debian13:nonroot` (digest-pinned),
  user `65532`, entrypoint `/bluebox-otelcol`. It has no `RUN` step, so
  buildx builds `linux/amd64` and `linux/arm64` without QEMU.

Workflows (repository root `.github/workflows/`):

- **`ci.yml` (pull request / push to `main`):**
  - builds `linux/amd64` and loads it;
  - runs the image-scan gate (`scripts/ci/image-scan-gate.sh`, allowlist
    `image-scan-allowlist.json`);
  - runs `validate` on a smoke config;
  - hands the image to the values tests and the kind e2e.
- **`release.yml` (tag `v*`):**
  - pushes the multi-arch image to
    `ghcr.io/pawelsiwek/bluebox-otelcol:<version>` (the tag without the
    leading `v`) with a BuildKit SPDX SBOM attestation (`sbom: true`);
  - signs SLSA build provenance with `actions/attest-build-provenance`
    (Sigstore, `push-to-registry: true`).

  Verify with `gh attestation verify oci://ghcr.io/pawelsiwek/bluebox-otelcol:<version> --repo pawelsiwek/bluebox-k8s-collector`.

**Vulnerability scanning:** `vuln-scan.yml` generates the distribution
sources and runs `govulncheck` over the whole binary daily, and scans the
latest published image with Trivy. The image scan in CI blocks a fixable
CRITICAL/HIGH finding before a release.

## Bumping

1. Vendor the new Dynatrace tag's `manifest.yaml` as
   `build/dynatrace-manifest-<tag>.yaml`, and update `pinnedManifest` in
   `build/manifest_test.go`.
2. Apply the same three changes to `builder-config.yaml`.
3. Move the module's `go.opentelemetry.io/collector/*` pins, the
   `groupbyattrsprocessor` version and `OCB_VERSION` in the `Dockerfile` to
   the manifest's release. Then run `GOWORK=off go mod tidy && go test ./...`.

Renovate leaves the collector pins alone for this reason (`renovate.json`).
