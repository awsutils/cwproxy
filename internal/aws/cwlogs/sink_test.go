package cwlogs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/awsutils/cwproxy/internal/logging"
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
		AppName:         "cwproxy",
		MetricNamespace: "sniff2cw/cwproxy",
		StreamName:      "stream-1",
		FlushInterval:   10 * time.Millisecond,
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
				"Endpoint": "/health",
			},
		},
		{
			Name:  "StatusCode",
			Value: 1,
			Unit:  metrics.UnitCount,
			Dimensions: map[string]string{
				"Endpoint":   "/health",
				"HTTPStatus": "200",
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
	if !strings.Contains(message, "\"HTTPStatus\":\"200\"") {
		t.Fatalf("HTTPStatus dimension missing from CloudWatch log event: %q", message)
	}
}

func TestSinkPublishEmitsMetricOnlyEMFEvent(t *testing.T) {
	t.Parallel()

	client := &fakeLogsClient{}
	sink, err := New(context.Background(), client, "/app/log/cwproxy", Options{
		AppName:         "cwproxy",
		MetricNamespace: "sniff2cw/cwproxy",
		StreamName:      "stream-1",
		FlushInterval:   10 * time.Millisecond,
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
				"Endpoint": "http://127.0.0.1:8080/health",
			},
		},
		{
			Name:  "HealthLatency",
			Value: 2.5,
			Unit:  metrics.UnitMilliseconds,
			Dimensions: map[string]string{
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
}
