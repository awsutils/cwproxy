package cwlogs

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/awsutils/cwproxy/internal/logging"
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
		StreamName:    "stream-1",
		FlushInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	entry := logging.NewEntry(
		"cwproxy",
		logging.DirectionIngress,
		logging.Request{Path: "/health"},
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
}
