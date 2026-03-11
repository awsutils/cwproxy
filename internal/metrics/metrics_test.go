package metrics

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeClient struct {
	mu      sync.Mutex
	batches [][]Datum
	err     error
}

func (f *fakeClient) Put(_ context.Context, data []Datum) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	copied := append([]Datum(nil), data...)
	f.batches = append(f.batches, copied)
	return f.err
}

func TestAsyncPublisherFlushesBatchesOnClose(t *testing.T) {
	t.Parallel()

	client := &fakeClient{}
	publisher := NewAsyncPublisher(client, Options{
		FlushInterval: time.Hour,
		MaxBatchSize:  2,
	})

	err := publisher.Publish(context.Background(), []Datum{
		{Name: "one"},
		{Name: "two"},
		{Name: "three"},
	})
	if err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}

	closeContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := publisher.Close(closeContext); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()

	if len(client.batches) != 2 {
		t.Fatalf("batch count = %d, want 2", len(client.batches))
	}
	if len(client.batches[0]) != 2 || len(client.batches[1]) != 1 {
		t.Fatalf("batch sizes = %d and %d", len(client.batches[0]), len(client.batches[1]))
	}
}

func TestAsyncPublisherRejectsPublishAfterClose(t *testing.T) {
	t.Parallel()

	publisher := NewAsyncPublisher(&fakeClient{}, Options{})
	if err := publisher.Close(context.Background()); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if err := publisher.Publish(context.Background(), []Datum{{Name: "x"}}); err == nil {
		t.Fatal("expected Publish to fail after Close")
	}
}

func TestAsyncPublisherReturnsContextErrorBeforeQueueing(t *testing.T) {
	t.Parallel()

	publisher := NewAsyncPublisher(&fakeClient{}, Options{})
	defer func() {
		if err := publisher.Close(context.Background()); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := publisher.Publish(ctx, []Datum{{Name: "x"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish error = %v, want context.Canceled", err)
	}
}

func TestAsyncPublisherReportsQueueDrops(t *testing.T) {
	t.Parallel()

	reported := make([]string, 0, 1)
	publisher := &AsyncPublisher{
		client: &fakeClient{},
		queue:  make(chan Datum, 1),
		reporter: func(format string, args ...any) {
			reported = append(reported, format)
		},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}

	err := publisher.Publish(context.Background(), []Datum{
		{Name: "one"},
		{Name: "two"},
	})
	if err == nil {
		t.Fatal("expected Publish to fail when the queue is full")
	}
	if len(reported) == 0 || reported[0] != "dropping CloudWatch metrics because the queue is full (total dropped: %d)" {
		t.Fatalf("reporter messages = %#v", reported)
	}
}

func TestAsyncPublisherReportsClientPutFailures(t *testing.T) {
	t.Parallel()

	reported := make([]string, 0, 1)
	publisher := NewAsyncPublisher(&fakeClient{err: errors.New("put failed")}, Options{
		FlushInterval: 10 * time.Millisecond,
		MaxBatchSize:  1,
		Reporter: func(format string, args ...any) {
			reported = append(reported, format)
		},
	})

	if err := publisher.Publish(context.Background(), []Datum{{Name: "one"}}); err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}
	if err := publisher.Close(context.Background()); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if len(reported) == 0 || reported[0] != "failed to publish CloudWatch metrics: %v" {
		t.Fatalf("reporter messages = %#v", reported)
	}
}

func TestCloneDatumClonesDimensions(t *testing.T) {
	t.Parallel()

	original := Datum{
		Name: "RequestCount",
		Dimensions: map[string]string{
			"AppName": "cwproxy",
		},
	}
	cloned := cloneDatum(original)
	original.Dimensions["AppName"] = "changed"

	if cloned.Dimensions["AppName"] != "cwproxy" {
		t.Fatalf("cloned dimensions = %#v", cloned.Dimensions)
	}
}

type capturePublisher struct {
	mu   sync.Mutex
	data []Datum
	err  error
}

func (p *capturePublisher) Publish(_ context.Context, data []Datum) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.data = append(p.data, data...)
	return p.err
}

func (p *capturePublisher) Close(context.Context) error {
	return p.err
}

func TestMultiPublisherFanOutAndAggregatesErrors(t *testing.T) {
	t.Parallel()

	first := &capturePublisher{}
	second := &capturePublisher{err: errors.New("publish failed")}
	publisher := NewMultiPublisher(nil, first, second)

	err := publisher.Publish(context.Background(), []Datum{{Name: "RequestCount", Value: 1}})
	if err == nil || !errors.Is(err, second.err) {
		t.Fatalf("Publish error = %v", err)
	}

	first.mu.Lock()
	if len(first.data) != 1 {
		t.Fatalf("first publisher data = %#v", first.data)
	}
	first.mu.Unlock()

	if err := publisher.Close(context.Background()); err == nil || !errors.Is(err, second.err) {
		t.Fatalf("Close error = %v", err)
	}
}
