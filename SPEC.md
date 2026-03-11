## Introduction

`cwproxy` is an HTTP reverse proxy written in Go. It forwards traffic to a configured upstream application host and port, writes structured access logs to stdout and CloudWatch Logs, and emits EMF metrics to stdout and CloudWatch Logs.

When available, logs must include auto-detected AWS runtime metadata for EC2, ECS, and EKS.

Logs must also include stable 6-character structural hashes for request queries, request bodies, and response bodies, plus a combined top-level hash for the request/response structure as a whole.

The application must be fail-safe, robust, performance-optimized, and efficient by default. Every component should handle errors defensively, avoid process crashes whenever recovery is possible, and continue operating safely under unexpected conditions.

Use current, well-supported Go and infrastructure technologies where they provide clear operational value. Prefer designs that reduce latency, CPU usage, memory usage, and overall resource consumption without weakening reliability.

---

## Execution Modes

The application must support two runtime modes.

### Normal Mode

Normal mode is used when `cwproxy` is started without a target application command.

In normal mode:

- `cwproxy` must proxy traffic to the configured upstream application host and port using `APP_HOST` and `APP_PORT`.
- The default upstream target is `127.0.0.1:{APP_PORT}`, but `APP_HOST` may point at another reachable upstream host when needed.

### Inspector Mode

Inspector mode is used when `cwproxy` is started with a target application command after the `cwproxy` executable name.

Examples:

```bash
./cwproxy {cmd} {...args}
./cwproxy webapp --port 8080
```

In inspector mode:

- `cwproxy` must start the provided target application as a child process.
- `cwproxy` must pass the provided command-line arguments through to the target application unchanged.
- `cwproxy` must run the target application with environment variables available to the `cwproxy` process.
- After the target application is started, `cwproxy` must determine which local TCP port the child process is listening on and then proxy that port using the same request, log, metric, and health behavior as normal mode.

Port selection rules in inspector mode:

- If `APP_PORT` is explicitly configured, `APP_PORT` must override automatic port detection.
- If `APP_PORT` is not explicitly configured, `cwproxy` must automatically detect the child process `LISTEN` port.
- Automatic port detection must work on Linux, macOS, and Windows.
- Automatic port detection may use OS-specific socket inspection mechanisms equivalent to tools such as `ss`, `lsof`, `netstat`, or native platform APIs.
- Because some applications do not begin listening immediately, `cwproxy` must poll for the child process `LISTEN` port repeatedly at high frequency until at least one listening port is found.
- The polling interval for automatic port detection must be approximately `100ms`.
- If the child process opens multiple listening ports, `cwproxy` must choose the lowest port number.

Child-process lifecycle rules in inspector mode:

- `cwproxy` must forward received interrupt or termination signals to the child process as well.
- Signal forwarding requirements include signals such as `SIGTERM`.
- If the child process exits, `cwproxy` must flush any remaining logs and metrics before exiting.
- After flushing remaining telemetry, `cwproxy` must exit with the same exit code as the child process.

---

## Environment Variables

### `PROXY_PORT`

- Default: `8081`
- Port used by the proxy application for incoming traffic.

### `APP_NAME`

- Default: EKS deployment name, then ECS task family, then system hostname
- Application identifier used in log output and metric dimensions.

### `APP_HOST`

- Default: `127.0.0.1`
- Host used for the target application behind the reverse proxy.
- `APP_HOST` must be used with `APP_PORT` when building the upstream proxy target.
- `APP_HOST` must also be used as the default host for any `HEALTH_URLS` entry that omits a host.
- `APP_HOST` must contain only the host portion and must not include a scheme, path, query, or port.

### `APP_PORT`

- Default: `8080`
- Port used by the target application behind the reverse proxy.
- In inspector mode, an explicitly configured `APP_PORT` must override automatic child-process listen-port detection.

### `HEALTH_URLS`

- Default: `{APP_HOST}:{APP_PORT}/health`
- Comma-separated list of health check endpoints.
- Each entry may omit the protocol, host, or port. Missing values are resolved with the following defaults:
  - Protocol defaults to `http`.
  - Host defaults to `APP_HOST`.
  - Port defaults to `APP_PORT` for the first entry, `80` for later `http` entries, and `443` for `https` entries.

Examples:

| Input | Resolved |
| --- | --- |
| `/health` | `http://{APP_HOST}:{APP_PORT}/health` |
| `:8081/health` | `http://{APP_HOST}:8081/health` |
| `/health,some.alb.example.com/healthz` | `http://{APP_HOST}:{APP_PORT}/health` and `http://some.alb.example.com:80/healthz` |
| `/healthz,some.alb.example.com` | `http://{APP_HOST}:{APP_PORT}/healthz` and `http://some.alb.example.com:80/healthz` |
| `/healthz,https://some.alb.example.com` | `http://{APP_HOST}:{APP_PORT}/healthz` and `https://some.alb.example.com:443/healthz` |

### `LOG_GROUP_NAME`

- Default: `/app/log/{APP_NAME}`
- CloudWatch Logs group name used for log delivery.

### `HEALTH_LOG_GROUP_NAME`

- Default: `/app/log/{APP_NAME}/health`
- CloudWatch Logs group name used for health EMF events.
- This setting is independent from `LOG_GROUP_NAME` so health delivery can be separated operationally from traffic request logs.

### `AWS_REGION` / `AWS_DEFAULT_REGION`

- Default: unset
- `AWS_REGION` must be preferred when set.
- `AWS_DEFAULT_REGION` must be used when `AWS_REGION` is unset.
- If both are unset, the application must fall back to region detection from runtime metadata when possible.
- Runtime metadata fallback must support EC2 region detection and ECS region inference.
- Shared AWS credentials may still be loaded from standard AWS config files.
- If no region can be resolved from env vars or metadata, CloudWatch integration must stay disabled.

---

## Log Format

Logs are emitted after each request/response pair is matched.

- Stdout log entries are minified single-line JSON with a stable field order.
- Stdout traffic and health log entries must also carry EMF fields when metrics are emitted with the log event.
- CloudWatch request log entries use the same JSON payload, but insert a newline immediately after `_q` to improve readability in the CloudWatch console.
- CloudWatch traffic log entries may also include EMF metric fields and an `_aws` envelope in the same event.
- Requests coming through the reverse proxy whose path matches any resolved `HEALTH_URLS` path must not emit proxy traffic telemetry and must not be remapped into health-category telemetry. Only the internal health runner may emit health-category log and metric events.
- Health EMF events are written to `HEALTH_LOG_GROUP_NAME`, not `LOG_GROUP_NAME`.
- Health log entries must include the health probe response body when one is available.
- Health log entries must omit `_q` and `app_name`. Those fields are present only on traffic log entries.

Example stdout log entry:

```json
{
  "_q": "{APP_NAME} {method} {path} {status} {delay}ms",
  "_t": "TRAFFIC",
  "app_name": "{APP_NAME}",
  "aws_meta": {
    "ec2": {
      "instance_id": "i-1234567890",
      "region": "ap-northeast-2"
    },
    "ecs": {
      "cluster": "demo-cluster",
      "task_arn": "arn:aws:ecs:region:account:task/123"
    },
    "eks": {
      "cluster_name": "demo-eks",
      "namespace": "default",
      "pod_name": "cwproxy-123"
    }
  },
  "global_hash": "012345",
  "delay": 123.456,
  "request": {
    "time": 1700000000000,
    "host": "example.com",
    "port": 8080,
    "path": "/api/foo",
    "method": "POST",
    "url": "example.com:8080/api/foo?key=value",
    "queries": { "key": "value" },
    "queries_hash": "1f2e3d",
    "cookies": { "session": "abc" },
    "headers": { "Content-Type": "application/json" },
    "body": { "field": "value" },
    "body_hash": "0f1e2d"
  },
  "response": {
    "time": 1700000000123,
    "status": 200,
    "headers": { "Content-Type": "application/json" },
    "set_cookies": { "session": "xyz" },
    "body": { "result": "ok" },
    "body_hash": "abcdef"
  }
}
```

- `_q`: human-readable summary for quick filtering
- `_q` format: `{APP_NAME} {METHOD} {PATH} {STATUS} {DELAY}ms`
- If the request method is unavailable, `_q` must use `UNKNOWN`
- `_t`: log category discriminator. It must be `TRAFFIC` for normal proxied request logs and `HEALTH` for health probe logs emitted by the internal health runner
- `app_name`: traffic-log application identifier. Health log entries must not include this field.
- `delay`: elapsed time in milliseconds as a JSON number with exactly three decimal places
- `request.time` and `response.time`: Unix timestamps in milliseconds
- `body`: parsed as an object when `Content-Type` is `application/json` or `application/x-www-form-urlencoded`; otherwise stored as a raw string
- Truncated request or response bodies must be marked with `...(truncated)` instead of causing unbounded memory growth
- `aws_meta`: optional AWS runtime metadata with any detected EC2, ECS, and EKS details
- `queries_hash`: stable 6-character hash of the request query structure
- `request.body_hash` and `response.body_hash`: stable 6-character hashes of body structure based on keys and container shape only, ignoring scalar values
- `global_hash`: stable 6-character combined hash derived from `request.queries`, `request.body`, and `response.body`

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
- Health logs must include request details, response details, and the response body when available.
- Health logs and health metrics should be emitted together in the same stdout and CloudWatch Logs event when possible.

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

Raw binaries must be published directly. Do not wrap binary artifacts in `.zip` or `.tar.gz` archives.

Publish the built binaries directly at the GitHub Pages site root rather than under a nested downloads directory.

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
