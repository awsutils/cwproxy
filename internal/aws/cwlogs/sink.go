package cwlogs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cloudwatchlogstypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/smithy-go"
	"github.com/awsutils/cwproxy/internal/logging"
	"github.com/awsutils/cwproxy/internal/metrics"
)

type Client interface {
	CreateLogGroup(context.Context, *cloudwatchlogs.CreateLogGroupInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogGroupOutput, error)
	CreateLogStream(context.Context, *cloudwatchlogs.CreateLogStreamInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.CreateLogStreamOutput, error)
	PutLogEvents(context.Context, *cloudwatchlogs.PutLogEventsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.PutLogEventsOutput, error)
}

type Options struct {
	AppName                string
	TrafficMetricNamespace string
	HealthMetricNamespace  string
	StreamName             string
	QueueSize              int
	FlushInterval          time.Duration
	MaxBatchSize           int
	MaxBatchBytes          int
	MaxEventBytes          int
	Reporter               func(string, ...any)
}

type Sink struct {
	client                 Client
	appName                string
	logGroupName           string
	logStreamName          string
	trafficMetricNamespace string
	healthMetricNamespace  string
	queue                  chan enqueuedEvent
	flushInterval          time.Duration
	maxBatchSize           int
	maxBatchBytes          int
	maxEventBytes          int
	reporter               func(string, ...any)
	stop                   chan struct{}
	done                   chan struct{}
	closed                 atomic.Bool
	dropped                atomic.Uint64
	once                   sync.Once
}

type enqueuedEvent struct {
	message   string
	timestamp int64
}

func New(ctx context.Context, client Client, logGroupName string, options Options) (*Sink, error) {
	if client == nil {
		return nil, errors.New("cloudwatch logs client is required")
	}
	if logGroupName == "" {
		return nil, errors.New("log group name is required")
	}
	if options.StreamName == "" {
		return nil, errors.New("log stream name is required")
	}
	if options.QueueSize <= 0 {
		options.QueueSize = 1024
	}
	if options.FlushInterval <= 0 {
		options.FlushInterval = 2 * time.Second
	}
	if options.MaxBatchSize <= 0 {
		options.MaxBatchSize = 1000
	}
	if options.MaxBatchBytes <= 0 {
		options.MaxBatchBytes = 900 * 1024
	}
	if options.MaxEventBytes <= 0 {
		options.MaxEventBytes = 256 * 1024
	}

	if err := ensureLogResources(ctx, client, logGroupName, options.StreamName); err != nil {
		return nil, err
	}

	sink := &Sink{
		client:                 client,
		appName:                options.AppName,
		logGroupName:           logGroupName,
		logStreamName:          options.StreamName,
		trafficMetricNamespace: options.TrafficMetricNamespace,
		healthMetricNamespace:  options.HealthMetricNamespace,
		queue:                  make(chan enqueuedEvent, options.QueueSize),
		flushInterval:          options.FlushInterval,
		maxBatchSize:           options.MaxBatchSize,
		maxBatchBytes:          options.MaxBatchBytes,
		maxEventBytes:          options.MaxEventBytes,
		reporter:               options.Reporter,
		stop:                   make(chan struct{}),
		done:                   make(chan struct{}),
	}

	go sink.run()
	return sink, nil
}

func (s *Sink) Log(ctx context.Context, entry logging.Entry) error {
	if s.closed.Load() {
		return errors.New("cloudwatch logs sink is closed")
	}

	message, err := logging.MarshalForCloudWatch(entry)
	if err != nil {
		return err
	}
	return s.enqueueMessage(ctx, message, entryTimestamp(entry))
}

func (s *Sink) LogWithMetrics(ctx context.Context, entry logging.Entry, data []metrics.Datum) error {
	return s.logWithMetrics(ctx, entry, data, s.trafficMetricNamespace)
}

func (s *Sink) LogHealthWithMetrics(ctx context.Context, entry logging.Entry, data []metrics.Datum) error {
	return s.logWithMetrics(ctx, entry, data, s.healthMetricNamespace)
}

func (s *Sink) Publish(ctx context.Context, data []metrics.Datum) error {
	if len(data) == 0 {
		return nil
	}
	if s.healthMetricNamespace == "" {
		return errors.New("cloudwatch health metric namespace is not configured")
	}

	batches, err := partitionDatums(data, reservedMetricRootKeys)
	if err != nil {
		return err
	}

	timestamp := time.Now().UnixMilli()
	var combined error
	for _, batch := range batches {
		message, err := marshalMetricEvent(s.appName, logging.CategoryHealth, s.healthMetricNamespace, timestamp, batch, true)
		if err != nil {
			combined = errors.Join(combined, err)
			continue
		}
		combined = errors.Join(combined, s.enqueueMessage(ctx, message, timestamp))
	}
	return combined
}

func (s *Sink) logWithMetrics(ctx context.Context, entry logging.Entry, data []metrics.Datum, namespace string) error {
	if len(data) == 0 || namespace == "" {
		return s.Log(ctx, entry)
	}

	batches, err := partitionDatums(data, reservedLogRootKeys)
	if err != nil {
		return err
	}
	if len(batches) == 0 {
		return s.Log(ctx, entry)
	}

	timestamp := entryTimestamp(entry)
	message, err := marshalLogWithMetrics(entry, namespace, batches[0])
	if err != nil {
		return err
	}
	combined := s.enqueueMessage(ctx, message, timestamp)

	for _, batch := range batches[1:] {
		metricMessage, err := marshalMetricEvent(s.appName, entry.Type, namespace, timestamp, batch, true)
		if err != nil {
			combined = errors.Join(combined, err)
			continue
		}
		combined = errors.Join(combined, s.enqueueMessage(ctx, metricMessage, timestamp))
	}

	return combined
}

func (s *Sink) Close(ctx context.Context) error {
	s.once.Do(func() {
		s.closed.Store(true)
		close(s.stop)
	})

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return nil
	}
}

func (s *Sink) run() {
	defer close(s.done)

	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()

	batch := make([]cloudwatchlogstypes.InputLogEvent, 0, s.maxBatchSize)
	batchBytes := 0

	flush := func() {
		if len(batch) == 0 {
			return
		}
		_, err := s.client.PutLogEvents(context.Background(), &cloudwatchlogs.PutLogEventsInput{
			LogEvents:     batch,
			LogGroupName:  aws.String(s.logGroupName),
			LogStreamName: aws.String(s.logStreamName),
		})
		if err != nil && s.reporter != nil {
			s.reporter("failed to publish CloudWatch log events: %v", err)
		}
		batch = batch[:0]
		batchBytes = 0
	}

	appendEvent := func(event enqueuedEvent) {
		size := len(event.message) + 26
		if len(batch) >= s.maxBatchSize || batchBytes+size > s.maxBatchBytes {
			flush()
		}
		batch = append(batch, cloudwatchlogstypes.InputLogEvent{
			Message:   aws.String(event.message),
			Timestamp: aws.Int64(event.timestamp),
		})
		batchBytes += size
	}

	for {
		select {
		case event := <-s.queue:
			appendEvent(event)
		case <-ticker.C:
			flush()
		case <-s.stop:
			for {
				select {
				case event := <-s.queue:
					appendEvent(event)
				default:
					flush()
					return
				}
			}
		}
	}
}

func (s *Sink) noteDrop() {
	dropped := s.dropped.Add(1)
	if s.reporter != nil && dropped%100 == 1 {
		s.reporter("dropping CloudWatch log events because the queue is full (total dropped: %d)", dropped)
	}
}

func (s *Sink) enqueueMessage(ctx context.Context, message []byte, timestamp int64) error {
	if len(message) > s.maxEventBytes {
		s.noteDrop()
		if s.reporter != nil {
			s.reporter("dropping oversized CloudWatch log event (%d bytes)", len(message))
		}
		return errors.New("cloudwatch log event exceeds maximum size")
	}

	event := enqueuedEvent{
		message:   string(message),
		timestamp: timestamp,
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	select {
	case s.queue <- event:
		return nil
	default:
		s.noteDrop()
		return errors.New("cloudwatch logs queue is full")
	}
}

func ensureLogResources(ctx context.Context, client Client, logGroupName, logStreamName string) error {
	if _, err := client.CreateLogGroup(ctx, &cloudwatchlogs.CreateLogGroupInput{
		LogGroupName: aws.String(logGroupName),
	}); err != nil && !alreadyExists(err) {
		return fmt.Errorf("create log group: %w", err)
	}

	if _, err := client.CreateLogStream(ctx, &cloudwatchlogs.CreateLogStreamInput{
		LogGroupName:  aws.String(logGroupName),
		LogStreamName: aws.String(logStreamName),
	}); err != nil && !alreadyExists(err) {
		return fmt.Errorf("create log stream: %w", err)
	}

	return nil
}

func alreadyExists(err error) bool {
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		return apiError.ErrorCode() == "ResourceAlreadyExistsException"
	}
	return false
}
