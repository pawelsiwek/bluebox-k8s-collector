#!/usr/bin/env bash
# image-scan-gate.sh — blocking pre-push vulnerability gate for container images.
#
# Usage: image-scan-gate.sh <image-ref> <allowlist.json> [<baseline-image-ref>]
#
# Exit 1 if the image has a fixable CRITICAL/HIGH vulnerability not exempted by
# the allowlist ({"allow":[{id,package,severity,reason,issue}]}; an entry exempts
# one (id, package) pair and every field but severity is required). Unfixed findings are out of scope by design
# (--ignore-unfixed): nothing a rebuild can act on. Allowlist entries no longer
# reported are ::warning:: (stale; remove them). Fails closed on scanner errors
# (after 3 attempts), on a report that does not describe the image given, when the
# image is not in the local docker daemon, and when the tag moves during the scan.
# --image-src docker makes trivy read the same local tag `docker push` uploads.
#
# Optional baseline: the image currently published. A (id, package) pair that
# the baseline already carries is exposure the release does not introduce and holding
# the release does not remove, so it is a ::warning:: instead of a block. Only findings
# new in <image-ref> block. The baseline is pulled and scanned with the same pinned
# Trivy; if it cannot be pulled, scanned or validated the gate falls back to strict
# (as if no baseline were given) and never to "allow".
#
# Trivy is a pinned, checksum-verified release binary (install-trivy.sh next to this
# script) rather than aquasecurity/trivy-action. TRIVY_BIN and
# DOCKER_BIN override the scanner and docker for tests only: they are honoured under
# IMAGE_SCAN_GATE_TEST_MODE=1 and are a hard error otherwise, so nothing in a workflow
# can swap the verified binary.
set -euo pipefail

if [[ $# -lt 2 ]] || [[ $# -gt 3 ]]; then
  printf '%s\n' "usage: $0 <image-ref> <allowlist.json> [<baseline-image-ref>]" >&2
  exit 2
fi
image=$1
allowlist=$2
baseline_image=${3:-}

if ! jq -e '.allow | type == "array"' "$allowlist" >/dev/null 2>&1; then
  printf '%s\n' "::error::invalid image-scan allowlist: $allowlist (need {\"allow\": [...]})" >&2
  exit 2
fi
# A malformed entry must not silently exempt (or fail to exempt) anything.
if ! jq -e '.allow | all(.[];
      type == "object"
      and (.id | type == "string" and length > 0)
      and (.package | type == "string" and length > 0)
      and (.issue | type == "number" and . == floor and . > 0)
      and (.reason | type == "string" and (gsub("^\\s+|\\s+$"; "") | length > 0)))' \
  "$allowlist" >/dev/null 2>&1; then
  printf '%s\n' "::error::invalid image-scan allowlist entry in $allowlist: id and package (non-empty strings), issue (integer > 0) and reason (non-blank) are required" >&2
  exit 2
fi

# A set override outside test mode is an attempted bypass; fail loudly, never ignore it.
if [[ "${IMAGE_SCAN_GATE_TEST_MODE:-}" != "1" ]]; then
  for override in TRIVY_BIN DOCKER_BIN; do
    if [[ -n "${!override:-}" ]]; then
      printf '%s\n' "::error::$override is a test-only override and is refused outside IMAGE_SCAN_GATE_TEST_MODE=1" >&2
      exit 2
    fi
  done
  TRIVY_BIN=""
  DOCKER_BIN=""
fi

docker_bin="${DOCKER_BIN:-docker}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

trivy="${TRIVY_BIN:-}"
if [[ -z "$trivy" ]]; then
  if ! trivy="$(bash "$(dirname "$0")/install-trivy.sh")"; then
    printf '%s\n' "::error::could not install the pinned Trivy (scripts/ci/install-trivy.sh)" >&2
    exit 2
  fi
fi
# Trivy runs after a cd, so a relative TRIVY_BIN must be resolved first.
case "$trivy" in
  /*) ;;
  *) trivy="$(cd "$(dirname "$trivy")" && pwd)/$(basename "$trivy")" ;;
esac

# Ambient Trivy configuration (cwd trivy.yaml/.trivyignore, TRIVY_* env) could
# silently widen the exemptions or lower the severity bar, bypassing the allowlist.
# Run in the private work dir with explicit empty config/ignore files and with every
# inherited TRIVY_* variable cleared except the cache location.
: >"$work/empty-ignore"
: >"$work/empty-config.yaml"
unset_args=()
while IFS= read -r name; do
  [[ -n "$name" ]] && [[ "$name" != "TRIVY_CACHE_DIR" ]] && unset_args+=(-u "$name")
done < <(env | awk -F= '/^TRIVY_/ {print $1}')

if ! id_before=$("$docker_bin" image inspect --format '{{.Id}}' "$image"); then
  printf '%s\n' "::error::image $image is not in the local docker daemon; build it with load: true before scanning" >&2
  exit 1
fi

# scan_image <ref> <report-out>: --image-src docker scans the local tag, i.e. the image
# the later `docker push` uploads (or the pulled baseline), not a registry copy.
# Returns 1 after 3 failed attempts. Prints nothing fatal itself: the caller decides.
scan_image() {
  local ref=$1 out=$2 attempt
  for attempt in 1 2 3; do
    rm -f "$out"
    if (cd "$work" && env ${unset_args[@]+"${unset_args[@]}"} "$trivy" image --quiet --image-src docker \
      --scanners vuln --severity CRITICAL,HIGH --ignore-unfixed \
      --ignorefile "$work/empty-ignore" --config "$work/empty-config.yaml" \
      --format json --output "$out" --exit-code 0 "$ref"); then
      return 0
    fi
    if [[ "$attempt" -lt 3 ]]; then
      printf '%s\n' "::warning::trivy scan of $ref failed (attempt $attempt/3); retrying" >&2
      sleep "${IMAGE_SCAN_RETRY_SLEEP:-15}"
    fi
  done
  return 1
}

# validate_report <ref> <report>: an empty or partial report would read as "no findings";
# require proof the scan ran. Echoes the reason and returns 1 when the report is unusable.
validate_report() {
  local ref=$1 report=$2
  if [[ ! -s "$report" ]]; then
    printf '%s\n' "report is empty"
  elif ! jq -e '.SchemaVersion != null' "$report" >/dev/null 2>&1; then
    printf '%s\n' "report has no SchemaVersion"
  elif [[ "$(jq -r '.ArtifactName // ""' "$report")" != "$ref" ]]; then
    printf '%s\n' "report ArtifactName does not match $ref"
  elif ! jq -e '.Metadata.OS.Family | type == "string" and length > 0' "$report" >/dev/null 2>&1; then
    printf '%s\n' "report has no Metadata.OS.Family (OS not detected)"
  else
    return 0
  fi
  return 1
}

if ! scan_image "$image" "$work/report.json"; then
  printf '%s\n' "::error::trivy scan of $image failed after 3 attempts (DB download or scanner error); re-run the job" >&2
  exit 1
fi
if ! report_error=$(validate_report "$image" "$work/report.json"); then
  printf '%s\n' "::error::invalid trivy report for $image: $report_error" >&2
  exit 1
fi

# The tag must still name the image that was scanned, or the push would upload another one.
id_after=$("$docker_bin" image inspect --format '{{.Id}}' "$image" || true)
if [[ "$id_before" != "$id_after" ]]; then
  printf '%s\n' "::error::image $image changed during the scan (${id_before} -> ${id_after:-none}); not pushing an unscanned image" >&2
  exit 1
fi

allowed=$(jq -c '[.allow[] | {id, package}]' "$allowlist")
found=$(jq -c '[.Results[]?.Vulnerabilities[]? | {id: .VulnerabilityID, package: .PkgName}] | unique' "$work/report.json")

# Baseline: every failure path leaves baseline_found empty, i.e. strict.
baseline_found='[]'
if [[ "$baseline_image" = "$image" ]]; then
  # Pulling the same tag would replace the local image that is about to be pushed.
  printf '%s\n' "::warning::baseline image equals the scanned image; gating strictly" >&2
elif [[ -n "$baseline_image" ]]; then
  if ! "$docker_bin" pull --quiet "$baseline_image" >/dev/null 2>&1; then
    printf '%s\n' "::warning::baseline image $baseline_image could not be pulled; gating strictly (no carried-over findings)" >&2
  elif ! scan_image "$baseline_image" "$work/baseline.json"; then
    printf '%s\n' "::warning::baseline scan of $baseline_image failed; gating strictly (no carried-over findings)" >&2
  elif ! baseline_error=$(validate_report "$baseline_image" "$work/baseline.json"); then
    printf '%s\n' "::warning::invalid trivy report for baseline $baseline_image: $baseline_error; gating strictly" >&2
  else
    baseline_found=$(jq -c '[.Results[]?.Vulnerabilities[]? | {id: .VulnerabilityID, package: .PkgName}] | unique' "$work/baseline.json")
  fi
fi

# An allowlist entry is stale only if neither the image nor the baseline reports it.
jq -r --argjson found "$found" --argjson base "$baseline_found" \
  '.allow[] | . as $a | select(any(($found + $base)[]; .id == $a.id and .package == $a.package) | not)
   | "::warning::stale image-scan allowlist entry \(.id) \(.package) (#\(.issue)): no longer reported — remove it"' \
  "$allowlist"

# Non-allowlisted findings the baseline already carries: reported, not blocking.
jq -r --argjson allowed "$allowed" --argjson base "$baseline_found" \
  '[.Results[]?.Vulnerabilities[]?
    | . as $v | select(any($allowed[]; .id == $v.VulnerabilityID and .package == $v.PkgName) | not)
    | select(any($base[]; .id == $v.VulnerabilityID and .package == $v.PkgName))]
   | unique_by(.VulnerabilityID + "|" + .PkgName) | .[]
   | "::warning::carried-over finding already in the deployed baseline (not introduced by this release): \(.Severity) \(.VulnerabilityID) \(.PkgName) \(.InstalledVersion) -> \(.FixedVersion)"' \
  "$work/report.json"

blocking=$(jq -r --argjson allowed "$allowed" --argjson base "$baseline_found" \
  '[.Results[]?.Vulnerabilities[]?
    | . as $v | select(any($allowed[]; .id == $v.VulnerabilityID and .package == $v.PkgName) | not)
    | select(any($base[]; .id == $v.VulnerabilityID and .package == $v.PkgName) | not)]
   | unique_by(.VulnerabilityID + "|" + .PkgName) | .[]
   | "\(.Severity)\t\(.VulnerabilityID)\t\(.PkgName) \(.InstalledVersion) -> \(.FixedVersion)"' \
  "$work/report.json")

if [[ -n "$blocking" ]]; then
  printf '%s\n' "::error::fixable CRITICAL/HIGH vulnerabilities in $image (fix them, or allowlist with a tracking issue in $allowlist):"
  printf '%s\n' "$blocking"
  exit 1
fi
printf '%s\n' "image-scan-gate: $image clean (fixable CRITICAL/HIGH; $(jq '.allow | length' "$allowlist") allowlisted)"
