package health

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/awsutils/cwproxy/internal/logging"
	"github.com/awsutils/cwproxy/internal/metadata"
	"github.com/awsutils/cwproxy/internal/metrics"
)

const defaultCaptureBytes = 64 << 10

type Options struct {
	AppName         string
	Metadata        *metadata.Snapshot
	Interval        time.Duration
	Client          *http.Client
	Sink            logging.Sink
	MaxCaptureBytes int
	Now             func() time.Time
	Reporter        func(string, ...any)
}

type Runner struct {
	endpoints       []*url.URL
	appName         string
	metadata        *metadata.Snapshot
	publisher       metrics.Publisher
	interval        time.Duration
	client          *http.Client
	sink            logging.Sink
	now             func() time.Time
	maxCaptureBytes int
	reporter        func(string, ...any)
}

func NewRunner(endpoints []*url.URL, publisher metrics.Publisher, options Options) *Runner {
	interval := options.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if publisher == nil {
		publisher = metrics.NopPublisher{}
	}

	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	maxCaptureBytes := options.MaxCaptureBytes
	if maxCaptureBytes <= 0 {
		maxCaptureBytes = defaultCaptureBytes
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}

	return &Runner{
		endpoints:       append([]*url.URL(nil), endpoints...),
		appName:         options.AppName,
		metadata:        options.Metadata,
		publisher:       publisher,
		interval:        interval,
		client:          client,
		sink:            options.Sink,
		now:             now,
		maxCaptureBytes: maxCaptureBytes,
		reporter:        options.Reporter,
	}
}

func (r *Runner) Run(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil && r.reporter != nil {
			r.reporter("health runner panic recovered: %v", recovered)
		}
	}()

	r.probeAll(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.probeAll(ctx)
		}
	}
}

func (r *Runner) probeAll(ctx context.Context) {
	if len(r.endpoints) == 0 || (r.publisher == nil && r.sink == nil) {
		return
	}

	var waitGroup sync.WaitGroup
	waitGroup.Add(len(r.endpoints))

	for _, endpoint := range r.endpoints {
		current := endpoint
		go func() {
			defer waitGroup.Done()
			r.probeEndpoint(ctx, current)
		}()
	}

	waitGroup.Wait()
}

func (r *Runner) probeEndpoint(ctx context.Context, endpoint *url.URL) {
	start := r.now()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		r.emit(ctx, buildHealthEntry(r.appName, r.metadata, endpoint, start, start, logging.Response{
			Status: 0,
			Body:   err.Error(),
		}), 0, 0)
		if r.reporter != nil {
			r.reporter("failed to build health request for %s: %v", endpoint.String(), err)
		}
		return
	}

	response, err := r.client.Do(request)
	if err != nil {
		end := r.now()
		r.emit(ctx, buildHealthEntry(r.appName, r.metadata, endpoint, start, end, logging.Response{
			Status: 0,
			Body:   err.Error(),
		}), 0, float64(end.Sub(start))/float64(time.Millisecond))
		if r.reporter != nil {
			r.reporter("health check failed for %s: %v", endpoint.String(), err)
		}
		return
	}

	end := r.now()
	body, truncated, readErr := readBody(response.Body, r.maxCaptureBytes)
	_ = response.Body.Close()
	if readErr != nil && r.reporter != nil {
		r.reporter("failed to read health response body for %s: %v", endpoint.String(), readErr)
	}

	status := 0.0
	if response.StatusCode >= 200 && response.StatusCode < 400 {
		status = 1
	}

	responseBody := logging.ParseBody(response.Header.Get("Content-Type"), body, truncated)
	if responseBody == nil && readErr != nil {
		responseBody = readErr.Error()
	}

	entry := buildHealthEntry(r.appName, r.metadata, endpoint, start, end, logging.Response{
		Status:     response.StatusCode,
		Headers:    logging.NormalizeHeaders(response.Header),
		SetCookies: logging.NormalizeCookies(response.Cookies()),
		Body:       responseBody,
	})
	r.emit(ctx, entry, status, float64(end.Sub(start))/float64(time.Millisecond))
}

func (r *Runner) emit(ctx context.Context, entry logging.Entry, status, latency float64) {
	endpoint := entry.Request.URL
	data := []metrics.Datum{
		{
			Name:  "HealthStatus",
			Value: status,
			Unit:  metrics.UnitCount,
			Dimensions: map[string]string{
				"AppName":  r.appName,
				"Endpoint": endpoint,
			},
		},
		{
			Name:  "HealthLatency",
			Value: latency,
			Unit:  metrics.UnitMilliseconds,
			Dimensions: map[string]string{
				"AppName":  r.appName,
				"Endpoint": endpoint,
			},
		},
	}

	if healthMetricSink, ok := r.sink.(logging.HealthMetricSink); ok {
		if err := healthMetricSink.LogHealthWithMetrics(ctx, entry, data); err != nil && r.reporter != nil {
			r.reporter("failed to write health log entry: %v", err)
		}
		return
	}

	if r.sink != nil {
		if err := r.sink.Log(ctx, entry); err != nil && r.reporter != nil {
			r.reporter("failed to write health log entry: %v", err)
		}
	}
	if err := r.publisher.Publish(ctx, data); err != nil && r.reporter != nil {
		r.reporter("failed to publish health metrics for %s: %v", endpoint, err)
	}
}

func buildHealthEntry(appName string, metadata *metadata.Snapshot, endpoint *url.URL, start, end time.Time, response logging.Response) logging.Entry {
	host := endpoint.Hostname()
	port := defaultPort(endpoint)
	path := endpoint.Path
	if path == "" {
		path = "/"
	}

	entry := logging.NewHealthEntry(
		appName,
		logging.Request{
			Time:    start.UnixMilli(),
			Host:    host,
			Port:    port,
			Path:    path,
			Method:  http.MethodGet,
			URL:     endpoint.String(),
			Queries: logging.NormalizeValues(endpoint.Query()),
		},
		logging.Response{
			Time:       end.UnixMilli(),
			Status:     response.Status,
			Headers:    response.Headers,
			SetCookies: response.SetCookies,
			Body:       response.Body,
		},
		end.Sub(start),
	)
	entry.Metadata = metadata
	return entry
}

func readBody(body io.ReadCloser, maxCaptureBytes int) ([]byte, bool, error) {
	if body == nil {
		return nil, false, nil
	}

	if maxCaptureBytes <= 0 {
		maxCaptureBytes = defaultCaptureBytes
	}

	limited := io.LimitReader(body, int64(maxCaptureBytes)+1)
	payload, err := io.ReadAll(limited)
	if len(payload) > maxCaptureBytes {
		return payload[:maxCaptureBytes], true, err
	}
	return payload, false, err
}

func defaultPort(endpoint *url.URL) int {
	if endpoint == nil {
		return 0
	}
	if portText := endpoint.Port(); portText != "" {
		port, err := strconv.Atoi(portText)
		if err == nil {
			return port
		}
	}
	if strings.EqualFold(endpoint.Scheme, "https") {
		return 443
	}
	return 80
}
