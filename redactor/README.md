# k8s-mcp-redactor

A read-only, redacting proxy in front of `kubernetes-mcp-server`. It is the only path from Bluebox live read into a cluster (contract C2):

```
Function Executor → EdgeConnect → hostAliases k8s-mcp-<uid8>.bluebox.internal → 127.0.0.1:8080 (this sidecar) → kubernetes-mcp-server
```

```
k8s-mcp-redactor --listen 127.0.0.1:8080 --upstream http://<mcp-server-service>:8081/mcp
```

Both flags are the whole command line. `--upstream` is required, and anything else is refused.

## What it enforces

- **Tool allowlist.** Only the read-only tools in `policy.go` are allowed. `tools/list` is filtered to them, and each listed tool is stamped `readOnlyHint: true`. Any other `tools/call` is refused before it reaches upstream.
- **Secret and ConfigMap reads are refused,** whatever the casing or plural form of the kind.
- **Object results** (`pods_get`, `resources_*`, `events_list`, …) are redacted structurally per contract C1:
  - env values, `command`/`args`, probe header values, and `token`/`password`/`secret` fields are masked;
  - `kubectl.kubernetes.io/last-applied-configuration` is dropped.
- **`pods_log` results** are redacted line by line with patterns (`logredact.go`). Each match becomes `<redacted>`. The patterns cover:
  - bearer, basic and Api-Token authorization headers;
  - Dynatrace tokens;
  - JWTs;
  - AWS access key IDs and secret-key assignments;
  - GitHub and Slack tokens;
  - PEM private key blocks;
  - `password`/`passwd`/`secret`/`token`/`api_key`/`credential` assignments in env, logfmt, YAML, JSON and CLI style;
  - `user:pass@` in connection strings.

  The result carries `_meta["bluebox.redaction"] = "pattern-based"`.
- **It fails closed.** A response it cannot parse becomes a JSON-RPC error. A log line it cannot process is replaced by `<line dropped by redactor>`: invalid UTF-8, a line over 64 KiB, or anything after an unterminated private key block.

## Limitation: log redaction is pattern-based and best effort

Log redaction recognizes known secret formats and credential-named assignments only. A secret that an application prints in an unrecognized format passes through. Examples: a bare random string with no key name, or a credential under an unusual field name. Object redaction (C1) is structural and does not have this gap.

Treat `pods_log` output as possibly sensitive. Keep it opt-in per workspace.

## Tests

```bash
cd redactor
GOWORK=off go test -race ./...            # golden files in testdata/; regenerate with -update
```

Log fixtures hold `{{PLACEHOLDER}}`s. Tests fill them with credential-shaped values built at runtime, so no secret-looking literal is committed.

## Image

`ghcr.io/pawelsiwek/k8s-mcp-redactor:<version>`, built for `linux/amd64` and
`linux/arm64` by the release workflow, with an SBOM and Sigstore build
provenance like the collector image (`../collector/README.md` § "Image").
The `live-read` values overlay runs it as a sidecar of the EdgeConnect Pod.
The live-read path is an experiment: its end-to-end spike through
EdgeConnect has not been run yet.
