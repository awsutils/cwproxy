# TODO

## Implementation Plan

1. Bootstrap the Go project.
   Create `go.mod`, a `cmd/cwproxy` entrypoint, and `internal` packages for config, proxy, logging, metrics, health, and AWS adapters.

2. Define runtime configuration.
   Implement env parsing for `PROXY_PORT`, `APP_PORT`, `APP_NAME`, `HEALTH_URLS`, and `LOG_GROUP_NAME`, including the exact `HEALTH_URLS` default-resolution rules from the spec.

3. Build the reverse proxy core.
   Use `net/http` with `httputil.ReverseProxy`, targeting the app on `APP_PORT`, and add request/response capture so delay, body size, status, and payload metadata can be recorded.

4. Implement structured log generation.
   Emit minified JSON with stable field order to stdout first, including `_q`, request and response timestamps, headers, cookies, queries, parsed bodies, and delay in milliseconds.

5. Add CloudWatch integrations.
   Wrap CloudWatch Logs and CloudWatch Metrics behind interfaces so the app logic stays testable. Use AWS SDK for Go v2 and define batching and retry behavior explicitly.

6. Implement health checks.
   Resolve all configured health endpoints, probe them on an interval, and publish `HealthStatus` and `HealthLatency` per endpoint.

7. Add tests before distribution work.
   Cover config parsing, `HEALTH_URLS` resolution, body parsing, log serialization, proxy behavior, and AWS client adapters with mocks or fakes.

8. Add performance validation.
   Create a repeatable local performance test focused on reverse-proxy delay, using a Go benchmark or a small load-test script plus a documented command.

9. Add packaging and CI.
   Build cross-platform binaries in GitHub Actions, publish them through GitHub Pages, then build a multi-arch container image by copying prebuilt binaries into the image and publish to `ghcr.io/awsutils/cwproxy` on every push.

10. Wire in repo hygiene.
    Add `gofmt`, `golangci-lint`, test commands, and keep commits incremental with conventional commit messages.

## Recommended First Milestone

Get a local proxy running with stdout logs and tests before touching CloudWatch. That provides a stable core for AWS delivery and CI.
