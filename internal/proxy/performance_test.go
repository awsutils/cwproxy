package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/awsutils/cwproxy/internal/logging"
	"github.com/awsutils/cwproxy/internal/metrics"
)

func BenchmarkHandlerRoundTrip(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	targetURL, err := url.Parse(upstream.URL)
	if err != nil {
		b.Fatalf("Parse upstream URL: %v", err)
	}

	handler := New(targetURL, logging.NopSink{}, metrics.NopPublisher{}, Options{
		AppName:         "cwproxy",
		MaxCaptureBytes: 4096,
	})

	request := httptest.NewRequest(http.MethodGet, "http://example.com/bench", nil)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request.Clone(request.Context()))
	}
}

func TestReverseProxyDelayBudget(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = writer.Write([]byte("ok"))
	}))
	defer upstream.Close()

	targetURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("Parse upstream URL: %v", err)
	}

	handler := New(targetURL, logging.NopSink{}, metrics.NopPublisher{}, Options{
		AppName:         "cwproxy",
		MaxCaptureBytes: 4096,
	})

	const iterations = 200
	start := time.Now()
	for i := 0; i < iterations; i++ {
		request := httptest.NewRequest(http.MethodGet, "http://example.com/perf", nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("response code = %d, want 200", recorder.Code)
		}
	}

	average := time.Since(start) / iterations
	t.Logf("average reverse proxy delay: %s", average)
	if average > 25*time.Millisecond {
		t.Fatalf("average reverse proxy delay = %s, exceeds 25ms budget", average)
	}
}
