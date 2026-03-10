# cwproxy

`cwproxy` is a Go HTTP reverse proxy that forwards traffic to a local application, emits structured JSON access logs to stdout and CloudWatch Logs, and publishes CloudWatch Metrics for health, request counts, latency, payload sizes, and status codes.

The service is designed to be fail-safe and defensive by default:

- bounded request and response body capture
- panic recovery in request handling
- async, bounded CloudWatch delivery queues
- graceful degradation when AWS delivery is unavailable
- low-overhead proxying with reusable buffers and pooled transports

## Build

```bash
go build -o cwproxy ./cmd/cwproxy
```

On Windows:

```powershell
go build -o .\cwproxy.exe .\cmd\cwproxy
```

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `PROXY_PORT` | `8081` | Listening port for the reverse proxy |
| `APP_PORT` | `8080` | Port of the target application behind the proxy |
| `APP_NAME` | hostname | Application name used in log/metric naming |
| `HEALTH_URLS` | `127.0.0.1:{APP_PORT}/health` | Comma-separated health endpoints |
| `LOG_GROUP_NAME` | `/app/log/{APP_NAME}` | CloudWatch Logs group name |
| `AWS_REGION` / `AWS_DEFAULT_REGION` | none | AWS region used to enable CloudWatch delivery |

Important: the current runtime enables CloudWatch Logs and Metrics only when `AWS_REGION` or `AWS_DEFAULT_REGION` is present in the environment. Shared AWS credentials from `~/.aws/credentials` can still be used, but set the region env var explicitly when starting the proxy.

CloudWatch outputs:

- Logs group: `LOG_GROUP_NAME`
- Metrics namespace: `sniff2cw/{APP_NAME}`

## Local Run

The examples below start:

1. a tiny demo upstream app on `127.0.0.1:18080`
2. `cwproxy` on `127.0.0.1:18081`
3. a test request through the proxy

The demo app exposes:

- `GET /health`
- echo responses for other paths

### Windows PowerShell

Start the demo app:

```powershell
@'
const http = require("http");

const server = http.createServer(async (req, res) => {
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
});

server.listen(18080, "127.0.0.1", () => {
  console.log("demo app listening on 127.0.0.1:18080");
});
'@ | node -
```

In another PowerShell window, build and run the proxy:

```powershell
cd C:\Users\pmh\Source\cwproxy
go build -o .\cwproxy.exe .\cmd\cwproxy

$env:PROXY_PORT = "18081"
$env:APP_PORT = "18080"
$env:APP_NAME = "cwproxy-local-test"
$env:HEALTH_URLS = "/health"
$env:LOG_GROUP_NAME = "/app/log/cwproxy-local-test"
$env:AWS_REGION = "us-east-1"

.\cwproxy.exe
```

In a third PowerShell window, send traffic through the proxy:

```powershell
Invoke-WebRequest `
  -Uri "http://127.0.0.1:18081/hello?name=pmh" `
  -Method POST `
  -ContentType "application/json" `
  -Body '{"demo":true}' |
  Select-Object -ExpandProperty Content
```

Expected result:

- the request returns JSON from the demo app
- the proxy prints a minified JSON access log to stdout
- health metrics/logs are delivered to AWS when credentials and permissions are valid

### Linux

Start the demo app:

```bash
node - <<'EOF'
const http = require("http");

const server = http.createServer(async (req, res) => {
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
});

server.listen(18080, "127.0.0.1", () => {
  console.log("demo app listening on 127.0.0.1:18080");
});
EOF
```

In another shell, build and run the proxy:

```bash
cd /path/to/cwproxy
go build -o ./cwproxy ./cmd/cwproxy

export PROXY_PORT=18081
export APP_PORT=18080
export APP_NAME=cwproxy-local-test
export HEALTH_URLS=/health
export LOG_GROUP_NAME=/app/log/cwproxy-local-test
export AWS_REGION=us-east-1

./cwproxy
```

In a third shell, send traffic through the proxy:

```bash
curl \
  -X POST \
  -H 'Content-Type: application/json' \
  -d '{"demo":true}' \
  'http://127.0.0.1:18081/hello?name=pmh'
```

### macOS

The macOS flow is the same as Linux:

```bash
cd /path/to/cwproxy
go build -o ./cwproxy ./cmd/cwproxy

export PROXY_PORT=18081
export APP_PORT=18080
export APP_NAME=cwproxy-local-test
export HEALTH_URLS=/health
export LOG_GROUP_NAME=/app/log/cwproxy-local-test
export AWS_REGION=us-east-1

./cwproxy
```

Send a request:

```bash
curl \
  -X POST \
  -H 'Content-Type: application/json' \
  -d '{"demo":true}' \
  'http://127.0.0.1:18081/hello?name=pmh'
```

If you use shared AWS profiles, also set one of these before starting the proxy when needed:

```bash
export AWS_PROFILE=your-profile
export AWS_REGION=us-east-1
```

## Development Validation

Format, test, lint, and run the proxy delay check with:

```bash
gofmt -w cmd internal
go test ./...
golangci-lint run ./...
go test -run TestReverseProxyDelayBudget -v ./internal/proxy
```

Optional benchmark:

```bash
go test -bench BenchmarkHandlerRoundTrip -benchmem ./internal/proxy -run ^$
```
