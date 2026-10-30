#!/bin/sh
# Pin the values tree to one published release: collector image tag + digest,
# global.bluebox.collectorVersion and the redactor image digest.
#   pin-release.sh <version> <collector-digest> <redactor-digest>
# <version> is X.Y.Z (no leading v); digests are sha256:<64 hex>.
# The release workflow runs it on the tagged commit and pushes the result as tag
# values-v<version>, which is the tag the Bluebox install snippet references.
set -eu
[ $# -eq 3 ] || { echo "usage: $0 <version> <collector-digest> <redactor-digest>" >&2; exit 2; }
version=$1 collector=$2 redactor=$3
echo "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || { echo "bad version: $version" >&2; exit 2; }
for d in "$collector" "$redactor"; do
  echo "$d" | grep -Eq '^sha256:[0-9a-f]{64}$' || { echo "bad digest: $d" >&2; exit 2; }
  [ "$d" != "sha256:0000000000000000000000000000000000000000000000000000000000000000" ] || { echo "placeholder digest" >&2; exit 2; }
done
here=$(cd "$(dirname "$0")/../.." && pwd)
common="$here/values/common.yaml"
live="$here/values/overlays/live-read/cluster.yaml"
# Line edits, not `yq -i`: yq drops the blank lines that structure the files.
tmp=$(mktemp)
sed -e "s|^    collectorVersion: .*|    collectorVersion: \"$version\"|" \
    -e "s|^  tag: .*|  tag: \"$version\"|" \
    -e "s|^  digest: .*|  digest: $collector|" "$common" > "$tmp" && cat "$tmp" > "$common"
sed -e "s|^      redactorImage: .*|      redactorImage: ghcr.io/pawelsiwek/k8s-mcp-redactor@$redactor|" "$live" > "$tmp" && cat "$tmp" > "$live"
rm -f "$tmp"
# Fail when a line edit missed its key.
[ "$(yq '.global.bluebox.collectorVersion' "$common")" = "$version" ] &&
[ "$(yq '.image.tag' "$common")" = "$version" ] &&
[ "$(yq '.image.digest' "$common")" = "$collector" ] &&
[ "$(yq '.global.bluebox.liveRead.redactorImage' "$live")" = "ghcr.io/pawelsiwek/k8s-mcp-redactor@$redactor" ] ||
  { echo "pin-release.sh: a values key was not updated" >&2; exit 1; }
