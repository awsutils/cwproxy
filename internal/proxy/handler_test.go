package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/awsutils/cwproxy/internal/logging"
	"github.com/awsutils/cwproxy/internal/metrics"
)

type captureSink struct {
	mu      sync.Mutex
	entries []logging.Entry
}

func (s *captureSink) Log(_ context.Context, entry logging.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	return nil
}

func (s *captureSink) Close(context.Context) error {
	return nil
}

type captureMetricSink struct {
	mu      sync.Mutex
	entries []logging.Entry
	metrics [][]metrics.Datum
}

func (s *captureMetricSink) Log(_ context.Context, entry logging.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	return nil
}

func (s *captureMetricSink) LogWithMetrics(_ context.Context, entry logging.Entry, data []metrics.Datum) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	copied := append([]metrics.Datum(nil), data...)
	s.metrics = append(s.metrics, copied)
	return nil
}

func (s *captureMetricSink) Close(context.Context) error {
	return nil
}

type metricCapture struct {
	mu   sync.Mutex
	data []metrics.Datum
}

func (m *metricCapture) Publish(_ context.Context, data []metrics.Datum) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = append(m.data, data...)
	return nil
}

func (m *metricCapture) Close(context.Context) error {
	return nil
}

func TestHandlerCapturesExchangeAndPublishesMetrics(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("ReadAll returned error: %v", err)
		}
		if string(payload) != `{"field":"value"}` {
			t.Fatalf("request body = %q", payload)
		}

		writer.Header().Set("Content-Type", "application/json")
		http.SetCookie(writer, &http.Cookie{Name: "session", Value: "xyz"})
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"result":"ok"}`))
	}))
	defer upstream.Close()

	targetURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("Parse upstream URL: %v", err)
	}

	sink := &captureSink{}
	publisher := &metricCapture{}
	handler := New(targetURL, sink, publisher, Options{
		AppName:         "cwproxy",
		MaxCaptureBytes: 4096,
	})

	request := httptest.NewRequest(http.MethodPost, "http://example.com/api/foo?key=value", strings.NewReader(`{"field":"value"}`))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: "session", Value: "abc"})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("response code = %d, want %d", recorder.Code, http.StatusCreated)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.entries) != 1 {
		t.Fatalf("entry count = %d, want 1", len(sink.entries))
	}

	entry := sink.entries[0]
	if entry.Request.Host != "example.com" {
		t.Fatalf("request host = %q", entry.Request.Host)
	}
	if entry.Request.Port != 80 {
		t.Fatalf("request port = %d, want 80", entry.Request.Port)
	}
	if entry.Request.URL != "example.com:80/api/foo?key=value" {
		t.Fatalf("request url = %q", entry.Request.URL)
	}
	if entry.Summary == "" || !strings.Contains(entry.Summary, "POST /api/foo") {
		t.Fatalf("summary = %q", entry.Summary)
	}
	requestBody, ok := entry.Request.Body.(map[string]any)
	if !ok || requestBody["field"] != "value" {
		t.Fatalf("request body = %#v", entry.Request.Body)
	}
	if entry.Response.Status != http.StatusCreated {
		t.Fatalf("response status = %d", entry.Response.Status)
	}
	if entry.Response.SetCookies["session"] != "xyz" {
		t.Fatalf("set cookies = %#v", entry.Response.SetCookies)
	}
	responseBody, ok := entry.Response.Body.(map[string]any)
	if !ok || responseBody["result"] != "ok" {
		t.Fatalf("response body = %#v", entry.Response.Body)
	}

	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.data) != 10 {
		t.Fatalf("metric count = %d, want 10", len(publisher.data))
	}
	if !containsMetric(publisher.data, "RequestCount", map[string]string{"AppName": "cwproxy"}) {
		t.Fatalf("aggregate RequestCount metric missing: %#v", publisher.data)
	}
	if !containsMetric(publisher.data, "RequestCount", map[string]string{
		"AppName":  "cwproxy",
		"Endpoint": "/api/foo",
		"Method":   http.MethodPost,
	}) {
		t.Fatalf("request-scoped RequestCount metric missing: %#v", publisher.data)
	}
	if !containsMetric(publisher.data, "2XXStatusCode", map[string]string{"AppName": "cwproxy"}) {
		t.Fatalf("aggregate 2XXStatusCode metric missing: %#v", publisher.data)
	}
	if !containsMetric(publisher.data, "2XXStatusCode", map[string]string{
		"AppName":  "cwproxy",
		"Endpoint": "/api/foo",
		"Method":   http.MethodPost,
	}) {
		t.Fatalf("request-scoped 2XXStatusCode metric missing: %#v", publisher.data)
	}
}

func TestHandlerReturnsBadGatewayAndTracksErrors(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen returned error: %v", err)
	}
	address := listener.Addr().String()
	listener.Close()

	targetURL, err := url.Parse("http://" + address)
	if err != nil {
		t.Fatalf("Parse target URL: %v", err)
	}

	sink := &captureSink{}
	publisher := &metricCapture{}
	handler := New(targetURL, sink, publisher, Options{
		AppName: "cwproxy",
	})

	request := httptest.NewRequest(http.MethodGet, "http://example.com/fail", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("response code = %d, want %d", recorder.Code, http.StatusBadGateway)
	}

	sink.mu.Lock()
	entry := sink.entries[0]
	sink.mu.Unlock()
	if entry.Response.Status != http.StatusBadGateway {
		t.Fatalf("entry response status = %d", entry.Response.Status)
	}
	if entry.Summary == "" || !strings.Contains(entry.Summary, "GET /fail 502") {
		t.Fatalf("summary = %q", entry.Summary)
	}

	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.data) != 10 {
		t.Fatalf("metric count = %d, want 10", len(publisher.data))
	}
	if !containsMetric(publisher.data, "5XXStatusCode", map[string]string{"AppName": "cwproxy"}) {
		t.Fatalf("aggregate 5XXStatusCode metric missing: %#v", publisher.data)
	}
	if !containsMetric(publisher.data, "5XXStatusCode", map[string]string{
		"AppName":  "cwproxy",
		"Endpoint": "/fail",
		"Method":   http.MethodGet,
	}) {
		t.Fatalf("request-scoped 5XXStatusCode metric missing: %#v", publisher.data)
	}
}

func TestHandlerUsesMetricAwareSinkForCombinedEmission(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok"))
	}))
	defer upstream.Close()

	targetURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("Parse upstream URL: %v", err)
	}

	sink := &captureMetricSink{}
	publisher := &metricCapture{}
	handler := New(targetURL, sink, publisher, Options{
		AppName: "cwproxy",
	})

	request := httptest.NewRequest(http.MethodGet, "http://example.com/combined", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", recorder.Code)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.entries) != 1 {
		t.Fatalf("entry count = %d, want 1", len(sink.entries))
	}
	if len(sink.metrics) != 1 {
		t.Fatalf("metric batch count = %d, want 1", len(sink.metrics))
	}
	if len(sink.metrics[0]) != 10 {
		t.Fatalf("metric count = %d, want 10", len(sink.metrics[0]))
	}
	if !containsMetric(sink.metrics[0], "2XXStatusCode", map[string]string{"AppName": "cwproxy"}) {
		t.Fatalf("aggregate 2XXStatusCode metric missing: %#v", sink.metrics[0])
	}
	if !containsMetric(sink.metrics[0], "2XXStatusCode", map[string]string{
		"AppName":  "cwproxy",
		"Endpoint": "/combined",
		"Method":   http.MethodGet,
	}) {
		t.Fatalf("metric batch = %#v", sink.metrics[0])
	}

	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.data) != 0 {
		t.Fatalf("fallback publisher unexpectedly used: %#v", publisher.data)
	}
}

func containsMetric(data []metrics.Datum, name string, dimensions map[string]string) bool {
	for _, datum := range data {
		if datum.Name != name {
			continue
		}
		if sameDimensions(datum.Dimensions, dimensions) {
			return true
		}
	}
	return false
}

func sameDimensions(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}

	leftKeys := make([]string, 0, len(left))
	for key := range left {
		leftKeys = append(leftKeys, key)
	}
	slices.Sort(leftKeys)

	for _, key := range leftKeys {
		if left[key] != right[key] {
			return false
		}
	}
	return true
}
