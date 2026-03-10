package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/awsutils/cwproxy/internal/logging"
	"github.com/awsutils/cwproxy/internal/metrics"
)

type capturePublisher struct {
	mu   sync.Mutex
	data []metrics.Datum
}

func (p *capturePublisher) Publish(_ context.Context, data []metrics.Datum) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.data = append(p.data, data...)
	return nil
}

func (p *capturePublisher) Close(context.Context) error {
	return nil
}

type captureHealthMetricSink struct {
	mu      sync.Mutex
	entries []logging.Entry
	metrics [][]metrics.Datum
}

func (s *captureHealthMetricSink) Log(context.Context, logging.Entry) error {
	return nil
}

func (s *captureHealthMetricSink) LogHealthWithMetrics(_ context.Context, entry logging.Entry, data []metrics.Datum) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	s.metrics = append(s.metrics, append([]metrics.Datum(nil), data...))
	return nil
}

func (s *captureHealthMetricSink) Close(context.Context) error {
	return nil
}

func TestProbeAllPublishesSuccessAndFailure(t *testing.T) {
	t.Parallel()

	successServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer successServer.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen returned error: %v", err)
	}
	failedURL, err := url.Parse("http://" + listener.Addr().String() + "/health")
	if err != nil {
		t.Fatalf("Parse failed URL: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	successURL, err := url.Parse(successServer.URL + "/health")
	if err != nil {
		t.Fatalf("Parse success URL: %v", err)
	}

	publisher := &capturePublisher{}
	runner := NewRunner([]*url.URL{successURL, failedURL}, publisher, Options{
		AppName: "cwproxy",
		Client:  &http.Client{Timeout: 200 * time.Millisecond},
	})

	runner.probeAll(context.Background())

	publisher.mu.Lock()
	defer publisher.mu.Unlock()

	if len(publisher.data) != 4 {
		t.Fatalf("metric count = %d, want 4", len(publisher.data))
	}

	statuses := map[string]float64{}
	for _, datum := range publisher.data {
		if datum.Name == "HealthStatus" {
			if datum.Dimensions["AppName"] != "cwproxy" {
				t.Fatalf("AppName dimension = %q, want cwproxy", datum.Dimensions["AppName"])
			}
			statuses[datum.Dimensions["Endpoint"]] = datum.Value
		}
	}
	if statuses[successURL.String()] != 1 {
		t.Fatalf("success status = %v", statuses[successURL.String()])
	}
	if statuses[failedURL.String()] != 0 {
		t.Fatalf("failure status = %v", statuses[failedURL.String()])
	}
}

func TestProbeEndpointLogsResponseBodyWithMetrics(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	endpoint, err := url.Parse(server.URL + "/health")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	sink := &captureHealthMetricSink{}
	runner := NewRunner([]*url.URL{endpoint}, metrics.NopPublisher{}, Options{
		AppName: "cwproxy",
		Sink:    sink,
		Client:  server.Client(),
	})

	runner.probeEndpoint(context.Background(), endpoint)

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.entries) != 1 {
		t.Fatalf("entry count = %d, want 1", len(sink.entries))
	}
	if len(sink.metrics) != 1 {
		t.Fatalf("metric batch count = %d, want 1", len(sink.metrics))
	}

	entry := sink.entries[0]
	responseBody, ok := entry.Response.Body.(map[string]any)
	if !ok || responseBody["status"] != "ok" {
		t.Fatalf("response body = %#v", entry.Response.Body)
	}
	if entry.Request.URL != endpoint.String() {
		t.Fatalf("request URL = %q, want %q", entry.Request.URL, endpoint.String())
	}
}
