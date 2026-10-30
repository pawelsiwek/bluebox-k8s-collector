# Bluebox Kubernetes collector image (README.md § "Image"). Multi-arch via
# buildx: the builder stage runs natively and cross-compiles for TARGETARCH.
FROM --platform=$BUILDPLATFORM golang:1.26.10-bookworm@sha256:fe3da92a5cb2881e4aa75e625cb0b7c50571632fb4cf340cbe53e6ccdcffdd51 AS build
ARG TARGETOS=linux
ARG TARGETARCH
# OCB version = the collector core release the pinned Dynatrace manifest builds against.
ARG OCB_VERSION=v0.162.0
# Extra go build flags, e.g. -p=2 on a small builder VM (the full distribution OOMs at 4 GiB with default parallelism).
ARG GO_BUILD_FLAGS=""
WORKDIR /src/collector
RUN GOWORK=off go install go.opentelemetry.io/collector/cmd/builder@${OCB_VERSION}
COPY . .
# Generate sources only, then compile with reproducible flags for the target platform.
RUN GOWORK=off builder --config builder-config.yaml --skip-compilation
WORKDIR /src/collector/_build
RUN GOWORK=off CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build ${GO_BUILD_FLAGS} -trimpath -buildvcs=false -ldflags="-s -w" -o /out/bluebox-otelcol .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
ARG REVISION=unknown
LABEL org.opencontainers.image.title="bluebox-otelcol" \
      org.opencontainers.image.description="Bluebox Kubernetes collector: Dynatrace OTel distribution v0.58.1 + Kubernetes topology components" \
      org.opencontainers.image.source="https://github.com/pawelsiwek/bluebox-k8s-collector" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/bluebox-otelcol /bluebox-otelcol
COPY LICENSE NOTICE /licenses/
USER 65532:65532
ENTRYPOINT ["/bluebox-otelcol"]
