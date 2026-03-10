package cwlogs

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/awsutils/cwproxy/internal/logging"
	"github.com/awsutils/cwproxy/internal/metadata"
	"github.com/awsutils/cwproxy/internal/metrics"
)

type fakeLogsClient struct {
	createGroupCalls  int
	createStreamCalls int
	inputs            []*cloudwatchlogs.PutLogEventsInput
}

func (f *fakeLogsClient) CreateLogGroup(context.Context, *cloudwatchlogs.CreateLogGroupInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogGroupOutput, error) {
	f.createGroupCalls++
	return &cloudwatchlogs.CreateLogGroupOutput{}, nil
}

func (f *fakeLogsClient) CreateLogStream(context.Context, *cloudwatchlogs.CreateLogStreamInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogStreamOutput, error) {
	f.createStreamCalls++
	return &cloudwatchlogs.CreateLogStreamOutput{}, nil
}

func (f *fakeLogsClient) PutLogEvents(_ context.Context, input *cloudwatchlogs.PutLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error) {
	copied := *input
	copied.LogEvents = append([]types.InputLogEvent(nil), input.LogEvents...)
	f.inputs = append(f.inputs, &copied)
	return &cloudwatchlogs.PutLogEventsOutput{}, nil
}

func TestSinkInitializesAndFlushesEvents(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{}
	sink, err := New(context.Background(), client, "/app/log/cwproxy", Options{
		AppName:       "cwproxy",
		StreamName:    "stream-1",
		FlushInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	entry := logging.NewEntry(
		"cwproxy",
		logging.Request{Method: "GET", Path: "/health"},
		logging.Response{Status: 200},
		time.Millisecond,
	)

	if err := sink.Log(context.Background(), entry); err != nil {
		t.Fatalf("first Log returned error: %v", err)
	}
	if err := sink.Log(context.Background(), entry); err != nil {
		t.Fatalf("second Log returned error: %v", err)
	}

	closeContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := sink.Close(closeContext); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if client.createGroupCalls != 1 {
		t.Fatalf("CreateLogGroup calls = %d", client.createGroupCalls)
	}
	if client.createStreamCalls != 1 {
		t.Fatalf("CreateLogStream calls = %d", client.createStreamCalls)
	}
	if len(client.inputs) == 0 || len(client.inputs[0].LogEvents) != 2 {
		t.Fatalf("PutLogEvents inputs = %#v", client.inputs)
	}
	message := *client.inputs[0].LogEvents[0].Message
	if !strings.Contains(message, "\",\n\"app_name\"") {
		t.Fatalf("CloudWatch message missing summary newline: %q", message)
	}
}

func TestSinkLogWithMetricsEmbedsEMFInSingleEvent(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{}
	sink, err := New(context.Background(), client, "/app/log/cwproxy", Options{
		AppName:                "cwproxy",
		TrafficMetricNamespace: "app/traffic",
		StreamName:             "stream-1",
		FlushInterval:          10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	entry := logging.NewEntry(
		"cwproxy",
		logging.Request{Method: "GET", Path: "/health"},
		logging.Response{Status: 200},
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
	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if len(client.inputs) != 1 || len(client.inputs[0].LogEvents) != 1 {
		t.Fatalf("PutLogEvents inputs = %#v", client.inputs)
	}

	message := *client.inputs[0].LogEvents[0].Message
	if !strings.Contains(message, "\"_aws\"") {
		t.Fatalf("EMF envelope missing from CloudWatch log event: %q", message)
	}
	if !strings.Contains(message, "\"RequestCount\":1") {
		t.Fatalf("RequestCount missing from CloudWatch log event: %q", message)
	}
	if !strings.Contains(message, "\"Method\":\"GET\"") {
		t.Fatalf("Method dimension missing from CloudWatch log event: %q", message)
	}

	payload := decodeEvent(t, message)
	if payload["2XXStatusCode"] != float64(1) {
		t.Fatalf("2XXStatusCode root field = %#v, want 1", payload["2XXStatusCode"])
	}

	envelope := decodeEnvelope(t, payload)
	if len(envelope.CloudWatchMetrics) != 2 {
		t.Fatalf("directive count = %d, want 2", len(envelope.CloudWatchMetrics))
	}
	if envelope.CloudWatchMetrics[0].Namespace != "app/traffic" || envelope.CloudWatchMetrics[1].Namespace != "app/traffic" {
		t.Fatalf("unexpected namespaces: %#v", envelope.CloudWatchMetrics)
	}
	if got := envelope.CloudWatchMetrics[0].Dimensions; len(got) != 1 || len(got[0]) != 1 || got[0][0] != "AppName" {
		t.Fatalf("aggregate dimensions = %#v", got)
	}
	if got := envelope.CloudWatchMetrics[1].Dimensions; len(got) != 1 || strings.Join(got[0], ",") != "AppName,Endpoint,Method" {
		t.Fatalf("request dimensions = %#v", got)
	}
}

func TestSinkPublishEmitsMetricOnlyEMFEvent(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{}
	sink, err := New(context.Background(), client, "/app/log/cwproxy", Options{
		AppName:               "cwproxy",
		HealthMetricNamespace: "app/health",
		StreamName:            "stream-1",
		FlushInterval:         10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	err = sink.Publish(context.Background(), []metrics.Datum{
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
			Value: 2.5,
			Unit:  metrics.UnitMilliseconds,
			Dimensions: map[string]string{
				"AppName":  "cwproxy",
				"Endpoint": "http://127.0.0.1:8080/health",
			},
		},
	})
	if err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}
	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if len(client.inputs) != 1 || len(client.inputs[0].LogEvents) != 1 {
		t.Fatalf("PutLogEvents inputs = %#v", client.inputs)
	}

	message := *client.inputs[0].LogEvents[0].Message
	if !strings.Contains(message, "cwproxy METRIC HealthLatency,HealthStatus") {
		t.Fatalf("metric summary missing from CloudWatch log event: %q", message)
	}
	if !strings.Contains(message, "\"Endpoint\":\"http://127.0.0.1:8080/health\"") {
		t.Fatalf("Endpoint missing from CloudWatch log event: %q", message)
	}
	if !strings.Contains(message, "\"HealthLatency\":2.5") {
		t.Fatalf("HealthLatency missing from CloudWatch log event: %q", message)
	}
	if !strings.Contains(message, "\"_aws\"") {
		t.Fatalf("EMF envelope missing from CloudWatch metric event: %q", message)
	}

	payload := decodeEvent(t, message)
	envelope := decodeEnvelope(t, payload)
	if len(envelope.CloudWatchMetrics) != 1 {
		t.Fatalf("directive count = %d, want 1", len(envelope.CloudWatchMetrics))
	}
	if envelope.CloudWatchMetrics[0].Namespace != "app/health" {
		t.Fatalf("namespace = %q, want app/health", envelope.CloudWatchMetrics[0].Namespace)
	}
	if got := envelope.CloudWatchMetrics[0].Dimensions; len(got) != 1 || strings.Join(got[0], ",") != "AppName,Endpoint" {
		t.Fatalf("health dimensions = %#v", got)
	}
}

func TestSinkLogHealthWithMetricsEmbedsResponseBody(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{}
	sink, err := New(context.Background(), client, "/app/log/cwproxy/health", Options{
		AppName:               "cwproxy",
		HealthMetricNamespace: "app/health",
		StreamName:            "stream-1",
		FlushInterval:         10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	entry := logging.NewEntry(
		"cwproxy",
		logging.Request{
			Method: http.MethodGet,
			Path:   "/health",
			URL:    "http://127.0.0.1:8080/health",
		},
		logging.Response{
			Status: 200,
			Body:   map[string]any{"status": "ok"},
		},
		time.Millisecond,
	)
	entry.Metadata = &metadata.Snapshot{
		ECS: &metadata.ECS{
			Cluster:       "demo-cluster",
			ContainerName: "cwproxy",
		},
	}

	err = sink.LogHealthWithMetrics(context.Background(), entry, []metrics.Datum{
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
			Value: 2.5,
			Unit:  metrics.UnitMilliseconds,
			Dimensions: map[string]string{
				"AppName":  "cwproxy",
				"Endpoint": "http://127.0.0.1:8080/health",
			},
		},
	})
	if err != nil {
		t.Fatalf("LogHealthWithMetrics returned error: %v", err)
	}
	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if len(client.inputs) != 1 || len(client.inputs[0].LogEvents) != 1 {
		t.Fatalf("PutLogEvents inputs = %#v", client.inputs)
	}

	message := *client.inputs[0].LogEvents[0].Message
	if !strings.Contains(message, "\"response\":{\"time\":0,\"status\":200") {
		t.Fatalf("response section missing from health log event: %q", message)
	}
	if !strings.Contains(message, "\"body\":{\"status\":\"ok\"}") {
		t.Fatalf("response body missing from health log event: %q", message)
	}
	if !strings.Contains(message, "\"aws_meta\":{\"ecs\":{\"cluster\":\"demo-cluster\",\"container_name\":\"cwproxy\"}}") {
		t.Fatalf("metadata missing from health log event: %q", message)
	}
	if !strings.Contains(message, "\"_aws\"") {
		t.Fatalf("EMF envelope missing from health log event: %q", message)
	}
}

func decodeEvent(t *testing.T, message string) map[string]any {
	t.Helper()

	payload := map[string]any{}
	if err := json.Unmarshal([]byte(message), &payload); err != nil {
		t.Fatalf("failed to decode log event: %v", err)
	}
	return payload
}

func decodeEnvelope(t *testing.T, payload map[string]any) emfEnvelope {
	t.Helper()

	rawEnvelope, err := json.Marshal(payload["_aws"])
	if err != nil {
		t.Fatalf("failed to remarshal EMF envelope: %v", err)
	}

	var envelope emfEnvelope
	if err := json.Unmarshal(rawEnvelope, &envelope); err != nil {
		t.Fatalf("failed to decode EMF envelope: %v", err)
	}
	return envelope
}
