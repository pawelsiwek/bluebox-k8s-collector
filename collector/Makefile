# Bluebox Kubernetes collector — local build and checks (README.md).
SHELL := /bin/sh
ENGINE ?= $(shell command -v podman || command -v docker)
IMAGE ?= localhost/bluebox-otelcol:dev
OCB_VERSION ?= v0.162.0
GO_BUILD_FLAGS ?=
GOVULNCHECK_VERSION ?= v1.3.0
export GOWORK := off

.PHONY: test image validate generate govulncheck clean

test:
	go vet ./...
	go test -race ./...
	@test -z "$$(gofmt -l build connector processor)" || { gofmt -l build connector processor; exit 1; }

image:
	$(ENGINE) build --build-arg GO_BUILD_FLAGS="$(GO_BUILD_FLAGS)" --build-arg REVISION=$$(git rev-parse HEAD 2>/dev/null || echo local) -t $(IMAGE) .

# make validate CONFIG=path/to/relay.yaml — otelcol validate with the built image.
validate:
	@test -n "$(CONFIG)" || { echo "usage: make validate CONFIG=<collector config>"; exit 2; }
	$(ENGINE) run --rm -v "$(abspath $(CONFIG)):/cfg/config.yaml:ro" $(IMAGE) validate --config=/cfg/config.yaml

# Generate the distribution sources (no compile) into _build/.
generate:
	go run go.opentelemetry.io/collector/cmd/builder@$(OCB_VERSION) --config builder-config.yaml --skip-compilation

# Scan the whole generated distribution, not just this module's three components.
govulncheck: generate
	cd _build && go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

clean:
	rm -rf _build
