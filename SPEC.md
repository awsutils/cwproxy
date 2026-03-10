## Introduction

`cwproxy` is an HTTP reverse proxy written in Go. It forwards traffic to a local application, writes structured access logs to stdout and CloudWatch Logs, and emits CloudWatch metrics through Embedded Metric Format (EMF).

The application must be fail-safe, robust, performance-optimized, and efficient by default. Every component should handle errors defensively, avoid process crashes whenever recovery is possible, and continue operating safely under unexpected conditions.

Use current, well-supported Go and infrastructure technologies where they provide clear operational value. Prefer designs that reduce latency, CPU usage, memory usage, and overall resource consumption without weakening reliability.

---

## Environment Variables

### `PROXY_PORT`

- Default: `8081`
- Port used by the proxy application for incoming traffic.

### `APP_NAME`

- Default: system hostname
- Application identifier used in log output and metric dimensions.

### `APP_PORT`

- Default: `8080`
- Port used by the target application behind the reverse proxy.

### `HEALTH_URLS`

- Default: `127.0.0.1:{APP_PORT}/health`
- Comma-separated list of health check endpoints.
- Each entry may omit the protocol, host, or port. Missing values are resolved with the following defaults:
  - Protocol defaults to `http`.
  - Host defaults to `127.0.0.1`.
  - Port defaults to `APP_PORT` for the first entry, `80` for later `http` entries, and `443` for `https` entries.

Examples:

| Input | Resolved |
| --- | --- |
| `/health` | `http://127.0.0.1:{APP_PORT}/health` |
| `:8081/health` | `http://127.0.0.1:8081/health` |
| `/health,some.alb.example.com/healthz` | `http://127.0.0.1:{APP_PORT}/health` and `http://some.alb.example.com:80/healthz` |
| `/healthz,some.alb.example.com` | `http://127.0.0.1:{APP_PORT}/healthz` and `http://some.alb.example.com:80/healthz` |
| `/healthz,https://some.alb.example.com` | `http://127.0.0.1:{APP_PORT}/healthz` and `https://some.alb.example.com:443/healthz` |

### `LOG_GROUP_NAME`

- Default: `/app/log/{APP_NAME}`
- CloudWatch Logs group name used for log delivery.

### `HEALTH_LOG_GROUP_NAME`

- Default: `/app/log/{APP_NAME}/health`
- CloudWatch Logs group name used for health EMF events.
- This setting is independent from `LOG_GROUP_NAME` so health delivery can be separated operationally from traffic request logs.

### `AWS_REGION` / `AWS_DEFAULT_REGION`

- Default: unset
- Enables CloudWatch delivery when one of these variables is present.
- Shared AWS credentials may still be loaded from standard AWS config files, but region selection must be available in the environment for CloudWatch integration to start.

---

## Log Format

Logs are emitted after each request/response pair is matched.

- Stdout log entries are minified single-line JSON with a stable field order.
- CloudWatch request log entries use the same JSON payload, but insert a newline immediately after `_q` to improve readability in the CloudWatch console.
- CloudWatch traffic log entries may also include EMF metric fields and an `_aws` envelope in the same event.
- Health EMF events are written to `HEALTH_LOG_GROUP_NAME`, not `LOG_GROUP_NAME`.

Example stdout log entry:

```json
{
  "_q": "{APP_NAME} {method} {path} {status} {delay}ms",
  "app_name": "{APP_NAME}",
  "delay": 123.456,
  "request": {
    "time": 1700000000000,
    "host": "example.com",
    "port": 8080,
    "path": "/api/foo",
    "method": "POST",
    "url": "example.com:8080/api/foo?key=value",
    "queries": { "key": "value" },
    "cookies": { "session": "abc" },
    "headers": { "Content-Type": "application/json" },
    "body": { "field": "value" }
  },
  "response": {
    "time": 1700000000123,
    "status": 200,
    "headers": { "Content-Type": "application/json" },
    "set_cookies": { "session": "xyz" },
    "body": { "result": "ok" }
  }
}
```

- `_q`: human-readable summary for quick filtering
- `_q` format: `{APP_NAME} {METHOD} {PATH} {STATUS} {DELAY}ms`
- If the request method is unavailable, `_q` must use `UNKNOWN`
- `delay`: elapsed time in milliseconds as a JSON number with exactly three decimal places
- `request.time` and `response.time`: Unix timestamps in milliseconds
- `body`: parsed as an object when `Content-Type` is `application/json` or `application/x-www-form-urlencoded`; otherwise stored as a raw string
- Truncated request or response bodies must be marked with `...(truncated)` instead of causing unbounded memory growth

---

## Metrics

All CloudWatch metrics are emitted through EMF in CloudWatch Logs.

### Traffic Metrics

- Namespace: `app/traffic`
- Dimension sets:
  - `{AppName}`
  - `{AppName, Endpoint, Method}`

| Metric | Unit | Description |
| --- | --- | --- |
| `RequestCount` | Count | Total number of proxied HTTP requests |
| `Latency` | Milliseconds | End-to-end request/response delay |
| `RequestBodySize` | Bytes | Size of the captured request body |
| `ResponseBodySize` | Bytes | Size of the captured response body |
| `2XXStatusCode` | Count | Count of responses with status `200-299` |
| `4XXStatusCode` | Count | Count of responses with status `400-499` |
| `5XXStatusCode` | Count | Count of responses with status `500-599` |

Rules:

- `RequestCount`, `Latency`, `RequestBodySize`, and `ResponseBodySize` must be emitted for both traffic dimension sets.
- Exactly one of `2XXStatusCode`, `4XXStatusCode`, or `5XXStatusCode` must be emitted for both traffic dimension sets when the response falls into one of those ranges.
- Traffic request logs and traffic metrics must be emitted together in the same CloudWatch Logs event when possible.

### Health Metrics

- Namespace: `app/health`
- Dimension set:
  - `{AppName, Endpoint}`

| Metric | Unit | Description |
| --- | --- | --- |
| `HealthStatus` | Count (`1` = up, `0` = down) | Health check result for each endpoint |
| `HealthLatency` | Milliseconds | Health check response time for each endpoint |

Rules:

- `HealthStatus` and `HealthLatency` must always use the `{AppName, Endpoint}` dimension set.
- Health metrics are emitted as EMF metric-only CloudWatch Logs events.

---

## Distribution

### Binary Distribution

The application must be distributed for the following platforms:

- Linux AMD64
- Linux ARM64
- Windows AMD64
- Darwin AMD64
- Darwin ARM64

Provide a GitHub Actions workflow that builds these binaries and publishes them through GitHub Pages.

All distribution binaries must be built with `CGO_ENABLED=0`.

### Container Image Distribution

The container image must support the following platforms:

- linux/amd64
- linux/arm64

Provide a GitHub Actions workflow that publishes the container image to `ghcr.io/awsutils/cwproxy`.

To reduce CI/CD time, do not build the Go application inside the container Dockerfile. Instead, build the binaries in GitHub Actions first and copy the built artifacts into the container image.

All container-distribution Go builds must use `CGO_ENABLED=0`.

### Considerations

- Always build the Go application on the GitHub Actions runner, not inside Docker.
- Trigger all distribution workflows on every push. Do not require Git tags.
- Always design for graceful degradation and safe failure instead of process termination.
- Optimize for low overhead and sustainable runtime efficiency, especially under sustained traffic.
