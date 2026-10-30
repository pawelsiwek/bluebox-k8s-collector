# Shared by the container tests: a local HTTPS OTLP sink (tests/otlp_sink.py).
# Neither the Bluebox nor the Dynatrace image has a file exporter, so tests
# export with otlp_http (encoding json) to this sink instead.
#   . tests/lib_sink.sh; sink_start <workdir>   -> SINK_HOST SINK_PORT SINK_CERTS SINK_LOG
#   sink_exporter_yaml                           -> exporter block for the collector config
#   sink_logs_export > out.json                  -> one ExportLogsServiceRequest JSON per line
#   sink_stop
sink_start() {
  SINK_HOST=${HOST_ALIAS:-host.containers.internal}
  SINK_PORT=${SINK_PORT:-18445}
  SINK_CERTS="$1/certs"
  SINK_LOG="$1/sink.jsonl"
  mkdir -p "$SINK_CERTS"
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=$SINK_HOST" \
    -addext "subjectAltName=DNS:$SINK_HOST,DNS:localhost" \
    -keyout "$SINK_CERTS/key.pem" -out "$SINK_CERTS/ca.crt" >/dev/null 2>&1
  : > "$SINK_LOG"
  python3 -I "$(dirname "$0")/otlp_sink.py" "$SINK_PORT" "$SINK_CERTS/ca.crt" "$SINK_CERTS/key.pem" "$SINK_LOG" &
  SINK_PID=$!
  chmod -R a+rX "$SINK_CERTS"
}
sink_exporter_yaml() {
  printf 'exporters:\n  otlp_http/sink:\n    endpoint: https://%s:%s\n    encoding: json\n    compression: none\n    tls:\n      ca_file: /certs/ca.crt\n' "$SINK_HOST" "$SINK_PORT"
}
sink_logs_export() { jq -c 'select(.path == "/v1/logs") | .body' "$SINK_LOG"; }
sink_stop() { [ -n "${SINK_PID:-}" ] && kill "$SINK_PID" 2>/dev/null || true; }
