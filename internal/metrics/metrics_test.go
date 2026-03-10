package metrics

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeClient struct {
	mu      sync.Mutex
	batches [][]Datum
}

func (f *fakeClient) Put(_ context.Context, data []Datum) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	copied := append([]Datum(nil), data...)
	f.batches = append(f.batches, copied)
	return nil
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
