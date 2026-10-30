#!/bin/sh
# Write the collector config a rendered release actually runs: the `relay`
# ConfigMap merged with every inline `--config=yaml:<a>::<b>: <value>` source
# from the container args (used for the Bluebox-only components, see
# values/cluster.yaml). Prints the remaining non-config args (feature gates).
#   effective_config.sh <rendered.yaml> <out.yaml>
set -eu
rendered=$1 out=$2
yq 'select(.kind == "ConfigMap" and .data.relay != null) | .data.relay' "$rendered" > "$out"
[ -s "$out" ] || { echo "no collector config in $rendered" >&2; exit 1; }
args=$(mktemp)
trap 'rm -f "$args"' EXIT
yq -o=json -I=0 'select(.kind == "DaemonSet" or .kind == "Deployment") | select(.metadata.name | test("^bluebox-otelcol")) | .spec.template.spec.containers[0].args[]' "$rendered" > "$args"
while IFS= read -r a; do
  a=$(printf '%s' "$a" | jq -r .)
  case "$a" in
    --config=yaml:*)
      spec=${a#--config=yaml:}
      path=${spec%%: *}
      value=${spec#*: }
      yqpath=$(printf '%s' "$path" | awk -F'::' '{for (i = 1; i <= NF; i++) printf ".\"%s\"", $i}')
      VALUE="$value" yq -i "$yqpath = (strenv(VALUE) | from_yaml)" "$out" ;;
    --config=*) ;;
    *) printf '%s\n' "$a" ;;
  esac
done < "$args"
