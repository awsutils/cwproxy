# cwproxy

`cwproxy` is a small Go reverse proxy for applications that already run on the same host or in the same shared network namespace. It listens on `PROXY_PORT`, forwards traffic to `127.0.0.1:APP_PORT`, writes structured JSON logs to stdout, and can also deliver request logs, health logs, and EMF metrics to CloudWatch Logs.

The project is built around operational safety:

- bounded request and response body capture
- panic recovery around request handling
- async, bounded CloudWatch delivery
- graceful degradation when AWS is unavailable
- static release artifacts built with `CGO_ENABLED=0`

Detailed performance notes are in [PERFORMANCE.md](PERFORMANCE.md).

## Features

- Reverse proxies HTTP traffic to `http://127.0.0.1:{APP_PORT}`
- Emits minified JSON access logs to stdout
- Emits CloudWatch traffic logs and EMF metrics in the same log event
- Emits health logs and health EMF metrics to a separate log group
- Publishes traffic metrics under `app/traffic`
- Publishes health metrics under `app/health`
- Auto-detects EC2, ECS, and EKS runtime metadata and includes it in logs when available
- Adds 6-character structural hashes for query shape, request body shape, response body shape, and combined request/response shape
- Truncates captured bodies instead of allowing unbounded memory growth

## How It Works

`cwproxy` is not a general upstream router. The upstream target is always `127.0.0.1:{APP_PORT}`.

That means the normal deployment patterns are:

- host deployment beside a local application process
- sidecar-style deployment where the application shares the same network namespace

At runtime:

1. `cwproxy` listens on `PROXY_PORT`
2. it proxies requests to `127.0.0.1:APP_PORT`
3. it logs request and response details to stdout
4. if AWS region and credentials are available, it also writes CloudWatch traffic logs and health logs
5. health probes run on a fixed 30 second interval

## Log And Metric Outputs

Traffic log output:

- stdout: single-line minified JSON
- CloudWatch log group: `LOG_GROUP_NAME`

Health log output:

- CloudWatch log group: `HEALTH_LOG_GROUP_NAME`
- includes health response body when available

Traffic metrics:

- namespace: `app/traffic`
- dimensions: `{AppName}`
- dimensions: `{AppName, Endpoint, Method}`
- metrics: `RequestCount`, `Latency`, `RequestBodySize`, `ResponseBodySize`, `2XXStatusCode`, `4XXStatusCode`, `5XXStatusCode`

Health metrics:

- namespace: `app/health`
- dimensions: `{AppName, Endpoint}`
- metrics: `HealthStatus`, `HealthLatency`

Important detail:

- metrics are sent through CloudWatch Embedded Metric Format in CloudWatch Logs
- `cwproxy` does not use `cloudwatch:PutMetricData`

## Configuration

Environment variables:

| Variable | Default | Description |
| --- | --- | --- |
| `PROXY_PORT` | `8081` | Port that `cwproxy` listens on |
| `APP_PORT` | `8080` | Local upstream application port |
| `APP_NAME` | hostname, then `cwproxy` | Application name used in logs and metric dimensions |
| `HEALTH_URLS` | `127.0.0.1:{APP_PORT}/health` | Comma-separated health endpoints |
| `LOG_GROUP_NAME` | `/app/log/{APP_NAME}` | CloudWatch Logs group for traffic logs and traffic EMF |
| `HEALTH_LOG_GROUP_NAME` | `/app/log/{APP_NAME}/health` | CloudWatch Logs group for health logs and health EMF |
| `AWS_REGION` | unset | Enables AWS delivery when set |
| `AWS_DEFAULT_REGION` | unset | Alternative AWS region variable |

Current fixed runtime defaults:

- health interval: `30s`
- request body capture limit: `64 KiB`
- response body capture limit: `64 KiB`

CloudWatch behavior:

- if neither `AWS_REGION` nor `AWS_DEFAULT_REGION` is set, CloudWatch delivery is disabled
- stdout logging still works when CloudWatch delivery is disabled
- if AWS config loading or CloudWatch sink initialization fails, the proxy keeps serving traffic and continues logging to stdout
- AWS credentials still follow the normal AWS SDK default credential chain

## `HEALTH_URLS` Resolution Rules

Each `HEALTH_URLS` entry may omit scheme, host, port, or path.

Resolution rules:

- default scheme: `http`
- default host: `127.0.0.1`
- default port for the first `http` entry: `APP_PORT`
- default port for later `http` entries: `80`
- default port for `https` entries: `443`
- if a later entry omits the path, it inherits the first entry path

Examples:

| Input | Resolved |
| --- | --- |
| `/health` | `http://127.0.0.1:{APP_PORT}/health` |
| `:8081/health` | `http://127.0.0.1:8081/health` |
| `/health,some.alb.example.com/healthz` | `http://127.0.0.1:{APP_PORT}/health` and `http://some.alb.example.com:80/healthz` |
| `/healthz,some.alb.example.com` | `http://127.0.0.1:{APP_PORT}/healthz` and `http://some.alb.example.com:80/healthz` |
| `/healthz,https://some.alb.example.com` | `http://127.0.0.1:{APP_PORT}/healthz` and `https://some.alb.example.com:443/healthz` |

## Required AWS Permissions

When AWS delivery is enabled, the process needs CloudWatch Logs permissions for both the traffic log group and the health log group.

Required actions:

- `logs:CreateLogGroup`
- `logs:CreateLogStream`
- `logs:PutLogEvents`

Important implementation detail:

- startup always attempts to create the configured log groups and streams
- because of that, `CreateLogGroup` and `CreateLogStream` permissions are required by the current implementation even if the groups already exist

Minimal action list:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "logs:CreateLogGroup",
        "logs:CreateLogStream",
        "logs:PutLogEvents"
      ],
      "Resource": "*"
    }
  ]
}
```

Notes:

- no `cloudwatch:PutMetricData` permission is required
- EC2, ECS, and EKS metadata enrichment uses local metadata sources and does not require extra IAM permissions

## Network Requirements

Inbound:

- clients or load balancers must be able to reach `PROXY_PORT`

Outbound:

- `cwproxy` must be able to reach `127.0.0.1:APP_PORT`
- `cwproxy` must be able to reach every resolved `HEALTH_URLS` endpoint
- if AWS delivery is enabled, `cwproxy` must be able to reach the regional CloudWatch Logs endpoint

## Deployment

### Binary Deployment

Local build:

```bash
go build -o ./cwproxy ./cmd/cwproxy
```

Windows build:

```powershell
go build -o .\cwproxy.exe .\cmd\cwproxy
```

Release artifacts:

- GitHub Actions builds static binaries on every push
- platforms: `linux/amd64`, `linux/arm64`, `windows/amd64`, `darwin/amd64`, `darwin/arm64`
- release archives are published through GitHub Pages

Simple Linux `systemd` example:

```ini
[Unit]
Description=cwproxy
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/cwproxy
Environment=PROXY_PORT=8081
Environment=APP_PORT=8080
Environment=APP_NAME=my-app
Environment=HEALTH_URLS=/health
Environment=LOG_GROUP_NAME=/app/log/my-app
Environment=HEALTH_LOG_GROUP_NAME=/app/log/my-app/health
Environment=AWS_REGION=us-east-1
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

### Container Deployment

Published image:

- `ghcr.io/awsutils/cwproxy:latest`
- `ghcr.io/awsutils/cwproxy:sha-<git-sha>`

The container image:

- is built for `linux/amd64` and `linux/arm64`
- uses an Alpine base image
- installs CA certificates for HTTPS health checks and CloudWatch delivery
- runs as a non-root user
- copies prebuilt Linux binaries into the image
- does not compile Go code inside Docker

The container image expects the upstream app to be reachable on `127.0.0.1:{APP_PORT}` from inside the same network namespace.

Linux host-network example:

```bash
docker run --rm \
  --network host \
  -e PROXY_PORT=8081 \
  -e APP_PORT=8080 \
  -e APP_NAME=my-app \
  -e HEALTH_URLS=/health \
  -e LOG_GROUP_NAME=/app/log/my-app \
  -e HEALTH_LOG_GROUP_NAME=/app/log/my-app/health \
  -e AWS_REGION=us-east-1 \
  ghcr.io/awsutils/cwproxy:latest
```

Kubernetes guidance:

- run `cwproxy` as a sidecar when the application is in the same Pod
- point clients or the Service at the `cwproxy` container port
- keep the application listening on `127.0.0.1:{APP_PORT}` or otherwise reachable on loopback inside the Pod network namespace

## Local Run

The example below starts a tiny Node upstream app on `127.0.0.1:18080` and runs `cwproxy` on `127.0.0.1:18081`.

Start the demo app:

```powershell
@'
const http = require("http");

http.createServer(async (req, res) => {
  const chunks = [];
  for await (const chunk of req) {
    chunks.push(chunk);
  }

  const body = Buffer.concat(chunks).toString();
  const url = new URL(req.url, "http://127.0.0.1:18080");

  if (url.pathname === "/health") {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ status: "ok" }));
    return;
  }

  res.setHeader("Set-Cookie", "demo=upstream");
  res.writeHead(200, { "Content-Type": "application/json" });
  res.end(JSON.stringify({
    path: url.pathname,
    method: req.method,
    query: Object.fromEntries(url.searchParams),
    body
  }));
}).listen(18080, "127.0.0.1", () => {
  console.log("demo app listening on 127.0.0.1:18080");
});
'@ | node -
```

Run `cwproxy`:

```powershell
go build -o .\cwproxy.exe .\cmd\cwproxy

$env:PROXY_PORT = "18081"
$env:APP_PORT = "18080"
$env:APP_NAME = "cwproxy-local-test"
$env:HEALTH_URLS = "/health"
$env:LOG_GROUP_NAME = "/app/log/cwproxy-local-test"
$env:HEALTH_LOG_GROUP_NAME = "/app/log/cwproxy-local-test/health"
$env:AWS_REGION = "us-east-1"

.\cwproxy.exe
```

Send a test request:

```powershell
Invoke-WebRequest `
  -Uri "http://127.0.0.1:18081/hello?name=pmh" `
  -Method POST `
  -ContentType "application/json" `
  -Body '{"demo":true}' |
  Select-Object -ExpandProperty Content
```

Linux and macOS use the same environment variables:

```bash
go build -o ./cwproxy ./cmd/cwproxy

export PROXY_PORT=18081
export APP_PORT=18080
export APP_NAME=cwproxy-local-test
export HEALTH_URLS=/health
export LOG_GROUP_NAME=/app/log/cwproxy-local-test
export HEALTH_LOG_GROUP_NAME=/app/log/cwproxy-local-test/health
export AWS_REGION=us-east-1

./cwproxy
```

Unix test request:

```bash
curl \
  -X POST \
  -H 'Content-Type: application/json' \
  -d '{"demo":true}' \
  'http://127.0.0.1:18081/hello?name=pmh'
```

## Development Validation

Required local validation:

```bash
gofmt -w cmd internal
go test ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.2 run ./...
go test -run TestReverseProxyDelayBudget -v ./internal/proxy
go test -bench BenchmarkHandlerRoundTrip -benchmem ./internal/proxy -run ^$
```

CI workflows:

- [verify.yml](.github/workflows/verify.yml)
- [binaries.yml](.github/workflows/binaries.yml)
- [container.yml](.github/workflows/container.yml)
