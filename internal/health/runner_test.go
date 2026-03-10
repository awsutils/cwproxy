package health

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

type failingLogSink struct {
	err error
}

func (s failingLogSink) Log(context.Context, logging.Entry) error {
	return s.err
}

func (s failingLogSink) Close(context.Context) error {
	return nil
}

type failingPublisher struct {
	err error
}

func (p failingPublisher) Publish(context.Context, []metrics.Datum) error {
	return p.err
}

func (p failingPublisher) Close(context.Context) error {
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type errorReadCloser struct {
	readErr error
}

func (r *errorReadCloser) Read([]byte) (int, error) {
	return 0, r.readErr
}

func (r *errorReadCloser) Close() error {
	return nil
}

type partialErrorReadCloser struct {
	payload []byte
	readErr error
	read    bool
}

func (r *partialErrorReadCloser) Read(body []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	count := copy(body, r.payload)
	return count, r.readErr
}

func (r *partialErrorReadCloser) Close() error {
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

func TestProbeEndpointUsesReadErrorAsResponseBody(t *testing.T) {
	t.Parallel()

	endpoint, err := url.Parse("http://127.0.0.1:8080/health")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	sink := &captureHealthMetricSink{}
	reported := make([]string, 0, 1)
	runner := NewRunner([]*url.URL{endpoint}, metrics.NopPublisher{}, Options{
		AppName: "cwproxy",
		Sink:    sink,
		Client: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header: http.Header{
						"Content-Type": {"application/json"},
					},
					Body: &errorReadCloser{readErr: errors.New("read failed")},
				}, nil
			}),
		},
		Reporter: func(format string, args ...any) {
			reported = append(reported, format)
		},
	})

	runner.probeEndpoint(context.Background(), endpoint)

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.entries) != 1 {
		t.Fatalf("entry count = %d, want 1", len(sink.entries))
	}
	if sink.entries[0].Response.Body != "read failed" {
		t.Fatalf("response body = %#v, want read error", sink.entries[0].Response.Body)
	}
	if len(reported) == 0 || !strings.Contains(reported[0], "failed to read health response body") {
		t.Fatalf("reporter messages = %#v", reported)
	}
}

func TestEmitReportsSinkAndPublisherFailures(t *testing.T) {
	t.Parallel()

	reported := make([]string, 0, 2)
	endpoint, err := url.Parse("http://127.0.0.1:8080/health")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	runner := NewRunner([]*url.URL{endpoint}, failingPublisher{err: errors.New("publish failed")}, Options{
		AppName:  "cwproxy",
		Sink:     failingLogSink{err: errors.New("log failed")},
		Reporter: func(format string, args ...any) { reported = append(reported, format) },
	})

	runner.emit(context.Background(), buildHealthEntry("cwproxy", nil, endpoint, time.Unix(0, 0), time.Unix(0, 0), logging.Response{
		Status: http.StatusServiceUnavailable,
		Body:   "down",
	}), 0, 10)

	if len(reported) != 2 {
		t.Fatalf("reporter messages = %#v, want 2", reported)
	}
	if !strings.Contains(reported[0], "failed to write health log entry") {
		t.Fatalf("first reporter message = %q", reported[0])
	}
	if !strings.Contains(reported[1], "failed to publish health metrics") {
		t.Fatalf("second reporter message = %q", reported[1])
	}
}

func TestReadBodyTruncatesAndReturnsReadError(t *testing.T) {
	t.Parallel()

	body, truncated, err := readBody(&partialErrorReadCloser{
		payload: []byte("abcde"),
		readErr: errors.New("read failed"),
	}, 4)
	if err == nil {
		t.Fatal("expected readBody to return an error")
	}
	if string(body) != "abcd" {
		t.Fatalf("body = %q, want abcd", body)
	}
	if !truncated {
		t.Fatal("expected body to be marked truncated")
	}
}
