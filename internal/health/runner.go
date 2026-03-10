package health

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/awsutils/cwproxy/internal/metrics"
)

type Options struct {
	AppName  string
	Interval time.Duration
	Client   *http.Client
	Reporter func(string, ...any)
}

type Runner struct {
	endpoints []*url.URL
	appName   string
	publisher metrics.Publisher
	interval  time.Duration
	client    *http.Client
	reporter  func(string, ...any)
}

func NewRunner(endpoints []*url.URL, publisher metrics.Publisher, options Options) *Runner {
	interval := options.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	return &Runner{
		endpoints: append([]*url.URL(nil), endpoints...),
		appName:   options.AppName,
		publisher: publisher,
		interval:  interval,
		client:    client,
		reporter:  options.Reporter,
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
	if len(r.endpoints) == 0 || r.publisher == nil {
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
	start := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		r.publish(ctx, endpoint.String(), 0, 0)
		if r.reporter != nil {
			r.reporter("failed to build health request for %s: %v", endpoint.String(), err)
		}
		return
	}

	response, err := r.client.Do(request)
	if err != nil {
		r.publish(ctx, endpoint.String(), 0, float64(time.Since(start))/float64(time.Millisecond))
		if r.reporter != nil {
			r.reporter("health check failed for %s: %v", endpoint.String(), err)
		}
		return
	}
	defer response.Body.Close()

	status := 0.0
	if response.StatusCode >= 200 && response.StatusCode < 400 {
		status = 1
	}

	r.publish(ctx, endpoint.String(), status, float64(time.Since(start))/float64(time.Millisecond))
}

func (r *Runner) publish(ctx context.Context, endpoint string, status, latency float64) {
	err := r.publisher.Publish(ctx, []metrics.Datum{
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
	})
	if err != nil && r.reporter != nil {
		r.reporter("failed to publish health metrics for %s: %v", endpoint, err)
	}
}
