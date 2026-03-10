package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	if entry.Direction != logging.DirectionIngress {
		t.Fatalf("direction = %q", entry.Direction)
	}
	if entry.Request.Host != "example.com" {
		t.Fatalf("request host = %q", entry.Request.Host)
	}
	if entry.Request.Port != 80 {
		t.Fatalf("request port = %d, want 80", entry.Request.Port)
	}
	if entry.Request.URL != "example.com:80/api/foo?key=value" {
		t.Fatalf("request url = %q", entry.Request.URL)
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
	if len(publisher.data) != 5 {
		t.Fatalf("metric count = %d, want 5", len(publisher.data))
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

	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	foundErrorCount := false
	for _, datum := range publisher.data {
		if datum.Name == "ErrorCount" {
			foundErrorCount = true
		}
	}
	if !foundErrorCount {
		t.Fatal("expected ErrorCount metric")
	}
}
