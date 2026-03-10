package cwmetrics

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/awsutils/cwproxy/internal/metrics"
)

type fakeCloudWatchClient struct {
	inputs []*cloudwatch.PutMetricDataInput
}

func (f *fakeCloudWatchClient) PutMetricData(_ context.Context, input *cloudwatch.PutMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error) {
	f.inputs = append(f.inputs, input)
	return &cloudwatch.PutMetricDataOutput{}, nil
}

func TestPublisherSplitsRequestsIntoBatches(t *testing.T) {
	t.Parallel()

	client := &fakeCloudWatchClient{}
	publisher := New(client, "sniff2cw/app")

	data := make([]metrics.Datum, 0, 25)
	for index := 0; index < 25; index++ {
		data = append(data, metrics.Datum{
			Name:  "Latency",
			Value: float64(index),
			Unit:  metrics.UnitMilliseconds,
			Dimensions: map[string]string{
				"Endpoint":   "/test",
				"StatusCode": "200",
			},
		})
	}

	if err := publisher.Put(context.Background(), data); err != nil {
		t.Fatalf("Put returned error: %v", err)
	}

	if len(client.inputs) != 2 {
		t.Fatalf("request count = %d, want 2", len(client.inputs))
	}
	if got := len(client.inputs[0].MetricData); got != 20 {
		t.Fatalf("first batch size = %d, want 20", got)
	}
	if got := len(client.inputs[1].MetricData); got != 5 {
		t.Fatalf("second batch size = %d, want 5", got)
	}

	firstDatum := client.inputs[0].MetricData[0]
	if aws.ToString(firstDatum.Dimensions[0].Name) != "Endpoint" {
		t.Fatalf("first dimension name = %q, want Endpoint", aws.ToString(firstDatum.Dimensions[0].Name))
	}
	if aws.ToString(firstDatum.Dimensions[1].Name) != "StatusCode" {
		t.Fatalf("second dimension name = %q, want StatusCode", aws.ToString(firstDatum.Dimensions[1].Name))
	}
}
