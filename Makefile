# bluebox-k8s-collector — local checks. The values tests need helm, yq (v4),
# jq, python3, uv (PyYAML, jsonschema), openssl and podman or docker.
#   make test          Go tests, topology tests and every values test
#   make test-go       collector components + redactor (vet, race tests)
#   make test-values   values render/validate/static/redaction tests (needs the Bluebox image)
#   make e2e-kind      kind-based e2e (event dedup, rotation, PSA); see tests/e2e/
#   make image         build the collector image locally (collector/Makefile)
SHELL := /bin/sh
OUT   ?= $(CURDIR)/.out
CHART_VERSION ?= 0.177.0
export CHART_VERSION
# Python helpers need PyYAML / jsonschema; uv provides them without a venv.
PY ?= uv run --quiet --no-project --with pyyaml==6.0.2 --with jsonschema==4.23.0 python

# Every supported layering, role:overlay+overlay. Pod logs need a namespace
# allowlist, so render with one.
COMBOS := \
  agent: cluster: \
  agent:aks agent:eks agent:gke agent:gke+autopilot agent:autopilot agent:local+pod-logs \
  agent:aks+pod-logs agent:gke+autopilot+pod-logs agent:eks+ai agent:aks+pod-logs+ai \
  cluster:aks cluster:eks cluster:gke cluster:gke+autopilot cluster:autopilot cluster:local \
  cluster:eks+ai cluster:aks+live-read cluster:gke+autopilot+live-read cluster:local+live-read \
  cluster:aks+alerting \
  agent:dynatrace-image+eks agent:dynatrace-image+local+pod-logs \
  cluster:dynatrace-image cluster:dynatrace-image+eks+ai cluster:dynatrace-image+gke+autopilot+live-read

.PHONY: pinned test test-go test-topology test-values image redactor-image helm-repo render validate static schema redaction heartbeat probe snippet allowlist podlogs events preview e2e-kind clean

ENGINE ?= $(shell command -v podman || command -v docker)

test: test-go test-topology test-values

test-go:
	$(MAKE) -C collector test
	cd redactor && GOWORK=off go vet ./... && GOWORK=off CGO_ENABLED=1 go test -race ./...
	@test -z "$$(gofmt -l redactor)" || { gofmt -l redactor; exit 1; }

test-topology:
	@cd topology && $(PY) -m unittest test_extension_pin test_ca_public
	@cd topology/routing && $(PY) -m unittest test_merge_routing

# Every values test, both image variants. The Bluebox image must exist locally
# (BLUEBOX_IMAGE, default localhost/bluebox-otelcol:dev; docs/values.md "Image variants").
test-values: validate static allowlist schema snippet redaction events podlogs heartbeat probe

image:
	$(MAKE) -C collector image

redactor-image:
	$(ENGINE) build --build-arg REVISION=$$(git rev-parse HEAD 2>/dev/null || echo local) -t localhost/k8s-mcp-redactor:dev redactor

helm-repo:
	@helm repo list 2>/dev/null | grep -q '^open-telemetry' || helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts >/dev/null
	@helm repo update open-telemetry >/dev/null

# helm template every combo into $(OUT)/<role>_<overlays>.yaml
render: helm-repo
	@mkdir -p $(OUT)
	@set -e; for c in $(COMBOS); do \
	  role=$${c%%:*}; ov=$$(echo $${c#*:} | tr '+' ' '); \
	  f=$(OUT)/$$(echo "$$c" | tr ':+' '_-').yaml; \
	  EXTRA_SET="--set global.bluebox.podLogs.namespaces={demo,shop}" ./scripts/render.sh $$role $$f $$ov; \
	  echo "rendered $$c"; \
	done

# otelcol-k8s validate on every rendered collector config
validate: render
	@set -e; for f in $(OUT)/*.yaml; do \
	  printf 'validate %-48s ' "$$(basename $$f)"; \
	  ./scripts/validate.sh $$f > $$f.validate.log 2>&1 || { echo FAIL; cat $$f.validate.log; exit 1; }; \
	  if grep -q '^SKIP' $$f.validate.log; then echo SKIP; else echo ok; fi; \
	done

# Static policy checks on the rendered manifests: PSA restricted with logs off,
# no hostPort, no nodes/proxy, no Secret/ConfigMap read, pinned images, v1 gates.
static: render
	@$(PY) tests/static_checks.py $(OUT)
	@# The autopilot overlay is self-contained: alone it must render exactly like gke+autopilot.
	@for r in agent cluster; do cmp -s $(OUT)/$${r}_autopilot.yaml $(OUT)/$${r}_gke-autopilot.yaml \
	  || { echo "FAIL: overlays/autopilot/$$r.yaml alone differs from gke+autopilot"; exit 1; }; done; \
	  echo "PASS autopilot overlay is self-contained (renders equal to gke+autopilot)"

# Every allowlist entry matches a metric the receivers emit (v0.163.0 metadata).
allowlist: render
	@$(PY) tests/allowlist_check.py $(OUT)

# Same as static, but a zero placeholder image digest fails (release check).
pinned: render
	@REQUIRE_PINNED_DIGESTS=1 $(PY) tests/static_checks.py $(OUT)

schema:
	@$(PY) tests/schema_check.py

snippet:
	@python3 -I tests/snippet_check.py

# Both image variants: Bluebox (default) and the stock Dynatrace image.
redaction:
	@VARIANT=bluebox ./tests/redaction_test.sh
	@VARIANT=dynatrace ./tests/redaction_test.sh

events:
	@./tests/events_test.sh

podlogs:
	@./tests/podlogs_glob_test.sh

heartbeat:
	@./tests/heartbeat_rotation_test.sh

probe:
	@./tests/probe_test.sh

preview: render
	@$(PY) scripts/security_preview.py $(OUT)/agent_eks.yaml $(OUT)/cluster_eks.yaml

e2e-kind:
	@./tests/e2e/kind_e2e.sh

clean:
	rm -rf $(OUT)
