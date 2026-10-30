#!/usr/bin/env bash
# install-trivy.sh — install the pinned, checksum-verified Trivy release binary into
# .tools/ (git-ignored) and print its path. Used by image-scan-gate.sh and vuln-scan.yml.
# Bump TRIVY_VERSION and every sha256 together, taking each value from the release's
# published checksums file.
set -euo pipefail

TRIVY_VERSION=0.74.0
case "$(uname -s)-$(uname -m)" in
  Linux-x86_64)  asset="trivy_${TRIVY_VERSION}_Linux-64bit.tar.gz";  sha=2ae6fe3ee734b7fdf11335663e18c75ea12dccc76062f09f164a3b0f8be4371a ;;
  Linux-aarch64) asset="trivy_${TRIVY_VERSION}_Linux-ARM64.tar.gz";  sha=b94ce1976bbf3c15b514b605ee88be7c6d94a29be2302847ff01cb794d47aad5 ;;
  Darwin-x86_64) asset="trivy_${TRIVY_VERSION}_macOS-64bit.tar.gz";  sha=472816f6888dda689d075c30254d4210b4d1035acf365aa72332f584c2f60485 ;;
  Darwin-arm64)  asset="trivy_${TRIVY_VERSION}_macOS-ARM64.tar.gz";  sha=1caada5e0e2091909357c7525d3aa76f4b660b13821bc143b190c7483e31cc11 ;;
  *) printf '%s\n' "install-trivy.sh: unsupported platform $(uname -s)-$(uname -m)" >&2; exit 2 ;;
esac

root="$(cd "$(dirname "$0")/../.." && pwd)"
dir="$root/.tools/trivy-$TRIVY_VERSION"
bin="$dir/trivy"
if [[ -x "$bin" ]]; then
  printf '%s\n' "$bin"
  exit 0
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
curl -fsSL --retry 3 -o "$work/$asset" "https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}/${asset}" >&2
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$work/$asset" | awk '{print $1}')"
else
  got="$(shasum -a 256 "$work/$asset" | awk '{print $1}')"
fi
if [[ "$got" != "$sha" ]]; then
  printf '%s\n' "::error::checksum mismatch for $asset: got $got, want $sha" >&2
  exit 1
fi
tar -xzf "$work/$asset" -C "$work" trivy
mkdir -p "$dir"
install -m 0755 "$work/trivy" "$bin"
printf '%s\n' "$bin"
