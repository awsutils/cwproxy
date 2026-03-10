package logging

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/awsutils/cwproxy/internal/metrics"
)

type failingWriter struct {
	failAfter int
	writes    int
}

func (w *failingWriter) Write(body []byte) (int, error) {
	w.writes++
	if w.writes > w.failAfter {
		return 0, errors.New("write failed")
	}
	return len(body), nil
}

type failingSink struct {
	logErr   error
	closeErr error
}

func (s failingSink) Log(context.Context, Entry) error {
	return s.logErr
}

func (s failingSink) Close(context.Context) error {
	return s.closeErr
}

type failingMetricSink struct {
	failingSink
	metricErr error
	healthErr error
}

func (s failingMetricSink) LogWithMetrics(context.Context, Entry, []metrics.Datum) error {
	return s.metricErr
}

func (s failingMetricSink) LogHealthWithMetrics(context.Context, Entry, []metrics.Datum) error {
	return s.healthErr
}

func TestStdoutSinkReturnsWriterErrors(t *testing.T) {
	t.Parallel()

	entry := NewEntry(
		"cwproxy",
		Request{Method: "GET", Path: "/health"},
		Response{Status: 200},
		0,
	)

	sink := NewStdoutSink(&failingWriter{failAfter: 0})
	if err := sink.Log(context.Background(), entry); err == nil {
		t.Fatal("expected first write to fail")
	}

	sink = NewStdoutSink(&failingWriter{failAfter: 1})
	if err := sink.Log(context.Background(), entry); err == nil {
		t.Fatal("expected newline write to fail")
	}
}

func TestMultiSinkAggregatesErrorsAcrossOperations(t *testing.T) {
	t.Parallel()

	entry := NewEntry(
		"cwproxy",
		Request{Method: "GET", Path: "/health"},
		Response{Status: 200},
		0,
	)
	data := []metrics.Datum{{Name: "RequestCount", Value: 1}}

	sink := NewMultiSink(
		failingSink{
			logErr:   errors.New("log failed"),
			closeErr: errors.New("close failed"),
		},
		failingMetricSink{
			failingSink: failingSink{
				logErr:   errors.New("fallback log failed"),
				closeErr: errors.New("metric close failed"),
			},
			metricErr: errors.New("metric failed"),
			healthErr: errors.New("health failed"),
		},
	)

	if err := sink.Log(context.Background(), entry); err == nil || !strings.Contains(err.Error(), "log failed") {
		t.Fatalf("Log error = %v", err)
	}
	if err := sink.LogWithMetrics(context.Background(), entry, data); err == nil || !strings.Contains(err.Error(), "metric failed") {
		t.Fatalf("LogWithMetrics error = %v", err)
	}
	if err := sink.LogHealthWithMetrics(context.Background(), entry, data); err == nil || !strings.Contains(err.Error(), "health failed") {
		t.Fatalf("LogHealthWithMetrics error = %v", err)
	}
	if err := sink.Close(context.Background()); err == nil || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("Close error = %v", err)
	}
}

func TestMarshalForCloudWatchReturnsJSONError(t *testing.T) {
	t.Parallel()

	_, err := MarshalForCloudWatch(Entry{
		AppName: "cwproxy",
		Request: Request{
			Queries: map[string]any{},
			Cookies: map[string]any{},
			Headers: map[string]any{},
			Body:    math.Inf(1),
		},
		Response: Response{
			Headers:    map[string]any{},
			SetCookies: map[string]any{},
		},
	})
	if err == nil {
		t.Fatal("expected MarshalForCloudWatch to fail")
	}
}

func TestNewMultiSinkFiltersNilAndNopSinkSucceeds(t *testing.T) {
	t.Parallel()

	entry := NewEntry(
		"cwproxy",
		Request{Method: "GET", Path: "/health"},
		Response{Status: 200},
		0,
	)

	buffer := &bytes.Buffer{}
	sink := NewMultiSink(nil, NewStdoutSink(buffer), NopSink{})
	if err := sink.Log(context.Background(), entry); err != nil {
		t.Fatalf("Log returned error: %v", err)
	}
	if !strings.Contains(buffer.String(), "\"app_name\":\"cwproxy\"") {
		t.Fatalf("stdout log = %q", buffer.String())
	}
}
