# Performance Report

## Scope

This document records the current local performance baseline for `cwproxy` and the main resource limits implied by the implementation.

These numbers are not synthetic marketing numbers. They are the practical baseline from the current code on the local development machine, with the current test and benchmark suite.

Measurement date: 2026-03-10

Environment:

- OS: Windows
- Arch: `amd64`
- CPU: `13th Gen Intel(R) Core(TM) i5-13500`
- Go benchmark target: [internal/proxy](/C:/Users/pmh/Source/cwproxy/internal/proxy)
- Binary measured: [cwproxy.exe](/C:/Users/pmh/Source/cwproxy/cmd/cwproxy/main.go)

## Measured Baseline

### Request Path Cost

Command:

```bash
go test -bench BenchmarkHandlerRoundTrip -benchmem ./internal/proxy -run ^$
```

Observed results on 2026-03-10:

- sample 0: `64399 ns/op`, `16982 B/op`, `171 allocs/op`
- sample 1: `67010 ns/op`, `16962 B/op`, `171 allocs/op`
- validation rerun: `83848 ns/op`, `16878 B/op`, `171 allocs/op`

Interpretation:

- The current proxy handler itself costs about `64-84 us` per request in the benchmarked path on this machine.
- The benchmarked allocation cost is about `16.5-16.9 KiB` per request.
- Allocation count was stable at `171 allocs/op` across runs.
- This is application-layer proxy overhead only. It does not include real network latency, real upstream work, or CloudWatch service latency.

### Reverse Proxy Delay Budget

Command:

```bash
go test -run TestReverseProxyDelayBudget -v ./internal/proxy
```

Result:

- average reverse proxy delay: `112.108 us`

Interpretation:

- The current in-repo delay guard is still well under `1 ms`.
- This number is the local added proxy delay in the test harness, not end-user response time.

### Idle Process Cost

Local process sample with CloudWatch disabled:

- working set: `9.24 MB`
- private memory: `45.88 MB`
- CPU over a 5-second idle window: effectively `0`

Interpretation:

- Idle CPU usage is negligible.
- The process is lightweight when it is listening but not actively proxying traffic.
- Private memory is materially higher than working set on Windows, so working set is the better quick operational number for resident memory pressure.

## What Increases Memory

### Body Capture

Current request and response capture uses the default body limit from [config.go](/C:/Users/pmh/Source/cwproxy/internal/config/config.go):

- request capture limit: `64 KiB`
- response capture limit: `64 KiB`

The capture buffers are implemented in [capture.go](/C:/Users/pmh/Source/cwproxy/internal/proxy/capture.go) and truncate safely once the limit is reached.

Practical implication:

- each in-flight request can retain up to about `128 KiB` of captured payload data before truncation
- this is bounded and prevents unbounded memory growth from large bodies

That means:

- `100` concurrent in-flight requests can retain roughly `12.5 MiB` of captured request/response body bytes
- `1000` concurrent in-flight requests can retain roughly `125 MiB` of captured request/response body bytes

This is not extra queueing overhead. It is the upper bound for active body capture in the request path.

### CloudWatch Log Queueing

The CloudWatch log sink defaults in [sink.go](/C:/Users/pmh/Source/cwproxy/internal/aws/cwlogs/sink.go) are:

- queue size: `1024`
- flush interval: `2s`
- max event size: `256 KiB`
- max batch bytes: `900 KiB`

Practical implication:

- the sink is bounded and will start dropping when the queue is full
- it does not grow without limit under CloudWatch backpressure

Worst-case retained message payload per sink:

- `1024 * 256 KiB = 256 MiB`

Important caveat:

- that is a hard worst case, not a normal operating expectation
- real log events are usually much smaller than the configured maximum
- if both traffic and health CloudWatch sinks are enabled, there are two independent queues

Operationally, the major memory risk is not the idle process. It is sustained downstream log backpressure combined with unusually large structured log events.

## What Increases CPU

The main CPU drivers in the current design are:

- JSON parsing for request and response bodies when content type is JSON or form data
- structural hash generation for request query shape, request body shape, response body shape, and `global_hash`
- body normalization for headers, cookies, and query values
- JSON log marshaling
- EMF event assembly when CloudWatch delivery is enabled

The current implementation already avoids several common waste patterns:

- body capture is bounded
- proxy transport is reused
- reverse proxy buffering uses a pool
- CloudWatch delivery is asynchronous and bounded
- health probing is periodic and isolated from the main request path

## Realistic Runtime Expectations

### With CloudWatch Disabled

This is the lightest operating mode and is closest to the benchmark numbers above.

Expected behavior:

- idle CPU stays near zero
- idle resident memory stays low
- request-path overhead is dominated by capture, normalization, hashing, and JSON marshaling

### With CloudWatch Enabled

This adds:

- EMF log assembly
- asynchronous queueing
- CloudWatch API flushing
- possible queue retention during downstream slowdown

Expected behavior:

- CPU per request increases modestly because each request now builds EMF-capable CloudWatch events
- memory can rise during CloudWatch throttling or network impairment, but queue growth is bounded and degrades by dropping events instead of exhausting the process

### Under Large Bodies

Expected behavior:

- body capture cost rises until the `64 KiB` capture ceiling is reached
- beyond the limit, additional payload bytes are not retained in memory for logging
- response time still depends on the upstream application and network, but log capture memory stays bounded

## Capacity Interpretation

A rough interpretation of the current `64-84 us/op` benchmark window is:

- the proxy-only handler path consumes about `64-84 ms` of CPU time per `1000` requests
- in a purely local microbenchmark, one fully busy core would theoretically have room for roughly `12k-15k` such handler operations per second

This is only a directional planning number. Real throughput will be lower because production traffic also pays for:

- real socket I/O
- upstream application latency
- CloudWatch flushing when enabled
- scheduler effects
- request size and response size variance

## Coverage of the Current Numbers

These numbers do cover:

- current handler overhead
- local idle process footprint
- current reverse proxy delay guard
- current bounded memory design for captures and log queues

These numbers do not cover:

- sustained multi-minute load
- multi-core saturation behavior
- production network RTT
- AWS throttling scenarios with real CloudWatch service latency
- memory growth under thousands of concurrent active requests

## Recommended Production Guidance

- Keep `CaptureBodyLimit` at the current default unless larger bodies are operationally necessary.
- Monitor CloudWatch delivery health because backpressure is the main non-idle memory multiplier.
- Treat the current benchmark as a proxy-overhead floor, not an end-to-end SLA.
- For production sizing, validate with a sustained load test that includes your real upstream application, real body sizes, and CloudWatch enabled.

## Commands Used

```bash
go test -bench BenchmarkHandlerRoundTrip -benchmem ./internal/proxy -run ^$
go test -run TestReverseProxyDelayBudget -v ./internal/proxy
```

The idle process sample was also measured from a live local `cwproxy` process started without AWS delivery enabled.
