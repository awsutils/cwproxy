package cwlogs

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/awsutils/cwproxy/internal/logging"
	"github.com/awsutils/cwproxy/internal/metrics"
)

func TestStdoutSinkLogWithMetricsEmbedsEMFOnOneLine(t *testing.T) {
	t.Parallel()

	buffer := &bytes.Buffer{}
	sink := NewStdoutSink(buffer)

	entry := logging.NewEntry(
		"cwproxy",
		logging.Request{Method: http.MethodGet, Path: "/health"},
		logging.Response{Status: http.StatusOK},
		time.Millisecond,
	)

	data := []metrics.Datum{
		{
			Name:  "RequestCount",
			Value: 1,
			Unit:  metrics.UnitCount,
			Dimensions: map[string]string{
				"AppName": "cwproxy",
			},
		},
		{
			Name:  "Latency",
			Value: 1,
			Unit:  metrics.UnitMilliseconds,
			Dimensions: map[string]string{
				"AppName": "cwproxy",
			},
		},
		{
			Name:  "RequestBodySize",
			Value: 12,
			Unit:  metrics.UnitBytes,
			Dimensions: map[string]string{
				"AppName": "cwproxy",
			},
		},
		{
			Name:  "ResponseBodySize",
			Value: 24,
			Unit:  metrics.UnitBytes,
			Dimensions: map[string]string{
				"AppName": "cwproxy",
			},
		},
		{
			Name:  "2XXStatusCode",
			Value: 1,
			Unit:  metrics.UnitCount,
			Dimensions: map[string]string{
				"AppName": "cwproxy",
			},
		},
		{
			Name:  "RequestCount",
			Value: 1,
			Unit:  metrics.UnitCount,
			Dimensions: map[string]string{
				"AppName":  "cwproxy",
				"Endpoint": "/health",
				"Method":   "GET",
			},
		},
		{
			Name:  "Latency",
			Value: 1,
			Unit:  metrics.UnitMilliseconds,
			Dimensions: map[string]string{
				"AppName":  "cwproxy",
				"Endpoint": "/health",
				"Method":   "GET",
			},
		},
		{
			Name:  "RequestBodySize",
			Value: 12,
			Unit:  metrics.UnitBytes,
			Dimensions: map[string]string{
				"AppName":  "cwproxy",
				"Endpoint": "/health",
				"Method":   "GET",
			},
		},
		{
			Name:  "ResponseBodySize",
			Value: 24,
			Unit:  metrics.UnitBytes,
			Dimensions: map[string]string{
				"AppName":  "cwproxy",
				"Endpoint": "/health",
				"Method":   "GET",
			},
		},
		{
			Name:  "2XXStatusCode",
			Value: 1,
			Unit:  metrics.UnitCount,
			Dimensions: map[string]string{
				"AppName":  "cwproxy",
				"Endpoint": "/health",
				"Method":   "GET",
			},
		},
	}

	if err := sink.LogWithMetrics(context.Background(), entry, data); err != nil {
		t.Fatalf("LogWithMetrics returned error: %v", err)
	}

	message := strings.TrimSpace(buffer.String())
	if strings.Count(buffer.String(), "\n") != 1 {
		t.Fatalf("stdout output = %q, want a single trailing newline", buffer.String())
	}
	if strings.Contains(message, "\",\n\"app_name\"") {
		t.Fatalf("stdout EMF event unexpectedly contains CloudWatch formatting newline: %q", message)
	}
	if !strings.Contains(message, "\"_aws\"") {
		t.Fatalf("EMF envelope missing from stdout event: %q", message)
	}
	if !strings.Contains(message, "\"_t\":\"TRAFFIC\"") {
		t.Fatalf("traffic category missing from stdout event: %q", message)
	}

	payload := decodeEvent(t, message)
	envelope := decodeEnvelope(t, payload)
	if len(envelope.CloudWatchMetrics) != 2 {
		t.Fatalf("directive count = %d, want 2", len(envelope.CloudWatchMetrics))
	}
}

func TestStdoutSinkLogHealthWithMetricsIncludesResponseBody(t *testing.T) {
	t.Parallel()

	buffer := &bytes.Buffer{}
	sink := NewStdoutSink(buffer)

	entry := logging.NewHealthEntry(
		"cwproxy",
		logging.Request{
			Method: http.MethodGet,
			Path:   "/health",
			URL:    "http://127.0.0.1:8080/health",
		},
		logging.Response{
			Status: http.StatusOK,
			Body:   map[string]any{"status": "ok"},
		},
		time.Millisecond,
	)

	data := []metrics.Datum{
		{
			Name:  "HealthStatus",
			Value: 1,
			Unit:  metrics.UnitCount,
			Dimensions: map[string]string{
				"AppName":  "cwproxy",
				"Endpoint": "http://127.0.0.1:8080/health",
			},
		},
		{
			Name:  "HealthLatency",
			Value: 1,
			Unit:  metrics.UnitMilliseconds,
			Dimensions: map[string]string{
				"AppName":  "cwproxy",
				"Endpoint": "http://127.0.0.1:8080/health",
			},
		},
	}

	if err := sink.LogHealthWithMetrics(context.Background(), entry, data); err != nil {
		t.Fatalf("LogHealthWithMetrics returned error: %v", err)
	}

	message := strings.TrimSpace(buffer.String())
	if !strings.Contains(message, "\"body\":{\"status\":\"ok\"}") {
		t.Fatalf("health response body missing from stdout event: %q", message)
	}
	if !strings.Contains(message, "\"_aws\"") {
		t.Fatalf("EMF envelope missing from stdout health event: %q", message)
	}
	if !strings.Contains(message, "\"_t\":\"HEALTH\"") {
		t.Fatalf("health category missing from stdout health event: %q", message)
	}
	if !strings.Contains(message, "\"HealthStatus\":1") || !strings.Contains(message, "\"HealthLatency\":1") {
		t.Fatalf("health metrics missing from stdout health event: %q", message)
	}
}

func TestStdoutSinkPublishEmitsTrafficMetricOnlyEvent(t *testing.T) {
	t.Parallel()

	buffer := &bytes.Buffer{}
	sink := NewStdoutSink(buffer)

	err := sink.Publish(context.Background(), []metrics.Datum{
		{
			Name:  "RequestCount",
			Value: 1,
			Unit:  metrics.UnitCount,
			Dimensions: map[string]string{
				"AppName": "cwproxy",
			},
		},
	})
	if err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}

	message := strings.TrimSpace(buffer.String())
	if !strings.Contains(message, "\"_t\":\"TRAFFIC\"") {
		t.Fatalf("traffic category missing from metric-only stdout event: %q", message)
	}
	if !strings.Contains(message, "\"app_name\":\"cwproxy\"") {
		t.Fatalf("app_name missing from metric-only stdout event: %q", message)
	}
	if !strings.Contains(message, "\"_aws\"") || !strings.Contains(message, "\"RequestCount\":1") {
		t.Fatalf("metric-only stdout event missing EMF payload: %q", message)
	}
}
