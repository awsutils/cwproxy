package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestHandlerRetriesConfiguredFiveXXResponse(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		attempts int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		attempts++
		currentAttempt := attempts
		mu.Unlock()

		if currentAttempt == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write([]byte("retry"))
			return
		}

		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"status":"ok"}`))
	}))
	defer upstream.Close()

	targetURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("Parse upstream URL: %v", err)
	}

	sink := &captureSink{}
	publisher := &metricCapture{}
	handler := New(targetURL, sink, publisher, Options{
		AppName: "cwproxy",
		RetryPolicy: RetryPolicy{
			MaxAttempts:       2,
			InitialBackoff:    0,
			MaxBackoff:        0,
			BackoffMultiplier: 1,
			Methods:           []string{http.MethodGet},
			StatusCodes:       []int{http.StatusServiceUnavailable},
		},
	})

	request := httptest.NewRequest(http.MethodGet, "http://example.com/retry", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", recorder.Code)
	}

	mu.Lock()
	if attempts != 2 {
		t.Fatalf("attempt count = %d, want 2", attempts)
	}
	mu.Unlock()

	sink.mu.Lock()
	if len(sink.entries) != 1 {
		t.Fatalf("entry count = %d, want 1", len(sink.entries))
	}
	if sink.entries[0].Response.Status != http.StatusOK {
		t.Fatalf("entry response status = %d, want 200", sink.entries[0].Response.Status)
	}
	sink.mu.Unlock()

	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if !containsMetric(publisher.data, "2XXStatusCode", map[string]string{"AppName": "cwproxy"}) {
		t.Fatalf("aggregate 2XXStatusCode metric missing: %#v", publisher.data)
	}
	if containsMetric(publisher.data, "5XXStatusCode", map[string]string{"AppName": "cwproxy"}) {
		t.Fatalf("aggregate 5XXStatusCode metric unexpectedly present: %#v", publisher.data)
	}
}

func TestHandlerRetriesConfiguredPostWithBufferedBodyReplay(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		attempts int
		bodies   []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("ReadAll returned error: %v", err)
		}

		mu.Lock()
		attempts++
		bodies = append(bodies, string(payload))
		currentAttempt := attempts
		mu.Unlock()

		if currentAttempt == 1 {
			writer.WriteHeader(http.StatusBadGateway)
			_, _ = writer.Write([]byte("retry"))
			return
		}

		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"status":"created"}`))
	}))
	defer upstream.Close()

	targetURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("Parse upstream URL: %v", err)
	}

	handler := New(targetURL, &captureSink{}, &metricCapture{}, Options{
		AppName: "cwproxy",
		RetryPolicy: RetryPolicy{
			MaxAttempts:       2,
			InitialBackoff:    0,
			MaxBackoff:        0,
			BackoffMultiplier: 1,
			Methods:           []string{http.MethodPost},
			StatusCodes:       []int{http.StatusBadGateway},
			BodyBufferBytes:   1024,
		},
		MaxCaptureBytes: 4096,
	})

	const requestBody = `{"field":"value"}`
	request := httptest.NewRequest(http.MethodPost, "http://example.com/retry-post", strings.NewReader(requestBody))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("response code = %d, want 201", recorder.Code)
	}

	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Fatalf("attempt count = %d, want 2", attempts)
	}
	if len(bodies) != 2 || bodies[0] != requestBody || bodies[1] != requestBody {
		t.Fatalf("request bodies = %#v", bodies)
	}
}

func TestHandlerSkipsRetryWhenRequestBodyExceedsReplayLimit(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		attempts int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte("retry"))
	}))
	defer upstream.Close()

	targetURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("Parse upstream URL: %v", err)
	}

	handler := New(targetURL, &captureSink{}, &metricCapture{}, Options{
		AppName: "cwproxy",
		RetryPolicy: RetryPolicy{
			MaxAttempts:       2,
			InitialBackoff:    0,
			MaxBackoff:        0,
			BackoffMultiplier: 1,
			Methods:           []string{http.MethodPost},
			StatusCodes:       []int{http.StatusServiceUnavailable},
			BodyBufferBytes:   4,
		},
	})

	request := httptest.NewRequest(http.MethodPost, "http://example.com/retry-large", strings.NewReader("payload"))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want 503", recorder.Code)
	}

	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Fatalf("attempt count = %d, want 1", attempts)
	}
}
