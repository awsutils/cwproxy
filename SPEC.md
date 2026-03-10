## Introduction

`cwproxy` is an HTTP reverse proxy written in Go. It writes combined log entries to both stdout and CloudWatch Logs.

It also publishes high-frequency CloudWatch Metrics for endpoint health, request counts, latency, payload size, and HTTP status codes.

---

## Environment Variables

### `PROXY_PORT`

- Default: `8081`
- Port used by the proxy application for incoming traffic.

### `APP_NAME`

- Default: system hostname
- Application identifier used in CloudWatch Logs and CloudWatch Metrics.

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

---

## Log Format

Logs are emitted after each request/response pair is matched. Every entry is written to both stdout and CloudWatch Logs as minified JSON with a stable field order.

```json
{
  "_q": "{APP_NAME} {direction} {path} {status} {delay}ms",
  "app_name": "{APP_NAME}",
  "direction": "INGRESS | EGRESS",
  "delay": 123,
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
- `delay`: elapsed time in milliseconds between the request and response
- `request.time` and `response.time`: Unix timestamps in milliseconds
- `body`: parsed as an object when `Content-Type` is `application/json` or `application/x-www-form-urlencoded`; otherwise stored as a raw string

---

## Metrics

All metrics are published to CloudWatch Metrics under the namespace `sniff2cw/{APP_NAME}` and include at least the `Endpoint` dimension.

| Metric | Unit | Description |
| --- | --- | --- |
| `HealthStatus` | Count (`1` = up, `0` = down) | Health check result for each endpoint |
| `HealthLatency` | Milliseconds | Health check response time for each endpoint |
| `RequestCount` | Count | Total number of HTTP requests |
| `ErrorCount` | Count | Number of responses with status >= `400` |
| `Latency` | Milliseconds | End-to-end request/response delay |
| `RequestBodySize` | Bytes | Size of the request body |
| `ResponseBodySize` | Bytes | Size of the response body |
| `StatusCode` | Count | Request count grouped by HTTP status code |

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

### Container Image Distribution

The container image must support the following platforms:

- linux/amd64
- linux/arm64

Provide a GitHub Actions workflow that publishes the container image to `ghcr.io/awsutils/cwproxy`.

To reduce CI/CD time, do not build the Go application inside the container Dockerfile. Instead, build the binaries in GitHub Actions first and copy the built artifacts into the container image.

### Considerations

- Always build the Go application on the GitHub Actions runner, not inside Docker.
- Trigger all distribution workflows on every push. Do not require Git tags.
