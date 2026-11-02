#!/bin/sh
# Print the image to run for a rendered release: the collector container's
# image, except that the Bluebox image with the zero placeholder digest on main
# maps to a locally built one, BLUEBOX_IMAGE (default localhost/bluebox-otelcol:dev,
# built with `make -C collector image`).
#   collector_image.sh <rendered.yaml>
# Exit 3 when that local image is missing; callers skip only if
# ALLOW_MISSING_BLUEBOX_IMAGE=1.
set -eu
img=$(yq 'select(.kind == "DaemonSet" or .kind == "Deployment") | select(.metadata.name | test("^bluebox-otelcol")) | .spec.template.spec.containers[0].image' "$1" | head -n 1)
case "$img" in
  *@sha256:0000000000000000000000000000000000000000000000000000000000000000)
    local_img="${BLUEBOX_IMAGE:-localhost/bluebox-otelcol:dev}"
    engine=${CONTAINER_ENGINE:-$(command -v podman || command -v docker)}
    if ! "$engine" image inspect "$local_img" >/dev/null 2>&1; then
      echo "Bluebox image $local_img not available: build it (make -C collector image) or set BLUEBOX_IMAGE" >&2
      exit 3
    fi
    echo "$local_img" ;;
  *) echo "$img" ;;
esac
