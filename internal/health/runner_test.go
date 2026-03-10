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
	listener.Close()

	successURL, err := url.Parse(successServer.URL + "/health")
	if err != nil {
		t.Fatalf("Parse success URL: %v", err)
	}

	publisher := &capturePublisher{}
	runner := NewRunner([]*url.URL{successURL, failedURL}, publisher, Options{
		Client: &http.Client{Timeout: 200 * time.Millisecond},
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
