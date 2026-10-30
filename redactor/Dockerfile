# Runs in the customer cluster next to kubernetes-mcp-server; built per target
# platform from the build host's toolchain (multi-arch via buildx).
FROM --platform=$BUILDPLATFORM golang:1.26.10-bookworm@sha256:fe3da92a5cb2881e4aa75e625cb0b7c50571632fb4cf340cbe53e6ccdcffdd51 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /build
# Stdlib-only: no go.sum.
COPY go.mod ./
COPY *.go ./
RUN GOWORK=off CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/k8s-mcp-redactor .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
ARG REVISION=unknown
LABEL org.opencontainers.image.title="k8s-mcp-redactor" \
      org.opencontainers.image.description="Read-only, redacting MCP proxy in front of kubernetes-mcp-server for Bluebox live read" \
      org.opencontainers.image.source="https://github.com/pawelsiwek/bluebox-k8s-collector" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/k8s-mcp-redactor /usr/local/bin/k8s-mcp-redactor
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/k8s-mcp-redactor"]
