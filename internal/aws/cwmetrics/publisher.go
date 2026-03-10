package cwmetrics

import (
	"context"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cloudwatchtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/awsutils/cwproxy/internal/metrics"
)

type Client interface {
	PutMetricData(context.Context, *cloudwatch.PutMetricDataInput, ...func(*cloudwatch.Options)) (*cloudwatch.PutMetricDataOutput, error)
}

type Publisher struct {
	client       Client
	namespace    string
	maxBatchSize int
}

func New(client Client, namespace string) *Publisher {
	return &Publisher{
		client:       client,
		namespace:    namespace,
		maxBatchSize: 20,
	}
}

func (p *Publisher) Put(ctx context.Context, data []metrics.Datum) error {
	if p.client == nil || len(data) == 0 {
		return nil
	}

	for start := 0; start < len(data); start += p.maxBatchSize {
		end := start + p.maxBatchSize
		if end > len(data) {
			end = len(data)
		}

		batch := make([]cloudwatchtypes.MetricDatum, 0, end-start)
		for _, datum := range data[start:end] {
			batch = append(batch, cloudwatchtypes.MetricDatum{
				MetricName: aws.String(datum.Name),
				Value:      aws.Float64(datum.Value),
				Unit:       cloudwatchtypes.StandardUnit(datum.Unit),
				Dimensions: buildDimensions(datum.Dimensions),
			})
		}

		if _, err := p.client.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
			Namespace:  aws.String(p.namespace),
			MetricData: batch,
		}); err != nil {
			return err
		}
	}

	return nil
}

func buildDimensions(values map[string]string) []cloudwatchtypes.Dimension {
	if len(values) == 0 {
		return nil
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	dimensions := make([]cloudwatchtypes.Dimension, 0, len(keys))
	for _, key := range keys {
		dimensions = append(dimensions, cloudwatchtypes.Dimension{
			Name:  aws.String(key),
			Value: aws.String(values[key]),
		})
	}
	return dimensions
}
