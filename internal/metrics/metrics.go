package metrics

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

const (
	UnitCount        = "Count"
	UnitMilliseconds = "Milliseconds"
	UnitBytes        = "Bytes"
)

type Datum struct {
	Name       string
	Value      float64
	Unit       string
	Dimensions map[string]string
}

type Publisher interface {
	Publish(context.Context, []Datum) error
	Close(context.Context) error
}

type Client interface {
	Put(context.Context, []Datum) error
}

type Options struct {
	QueueSize     int
	FlushInterval time.Duration
	MaxBatchSize  int
	Reporter      func(string, ...any)
}

type AsyncPublisher struct {
	client        Client
	queue         chan Datum
	flushInterval time.Duration
	maxBatchSize  int
	reporter      func(string, ...any)
	stop          chan struct{}
	done          chan struct{}
	closed        atomic.Bool
	dropped       atomic.Uint64
	once          sync.Once
}

func NewAsyncPublisher(client Client, options Options) *AsyncPublisher {
	if options.QueueSize <= 0 {
		options.QueueSize = 1024
	}
	if options.FlushInterval <= 0 {
		options.FlushInterval = 5 * time.Second
	}
	if options.MaxBatchSize <= 0 {
		options.MaxBatchSize = 20
	}

	publisher := &AsyncPublisher{
		client:        client,
		queue:         make(chan Datum, options.QueueSize),
		flushInterval: options.FlushInterval,
		maxBatchSize:  options.MaxBatchSize,
		reporter:      options.Reporter,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}

	go publisher.run()
	return publisher
}

func (p *AsyncPublisher) Publish(ctx context.Context, data []Datum) error {
	if len(data) == 0 || p.client == nil {
		return nil
	}
	if p.closed.Load() {
		return errors.New("metrics publisher is closed")
	}

	var combined error
	for _, datum := range data {
		copyDatum := cloneDatum(datum)

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		select {
		case p.queue <- copyDatum:
		default:
			p.noteDrop()
			combined = errors.Join(combined, errors.New("metrics queue is full"))
		}
	}

	return combined
}

func (p *AsyncPublisher) Close(ctx context.Context) error {
	p.once.Do(func() {
		p.closed.Store(true)
		close(p.stop)
	})

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return nil
	}
}

type NopPublisher struct{}

func (NopPublisher) Publish(context.Context, []Datum) error {
	return nil
}

func (NopPublisher) Close(context.Context) error {
	return nil
}

type MultiPublisher struct {
	publishers []Publisher
}

func NewMultiPublisher(publishers ...Publisher) *MultiPublisher {
	filtered := make([]Publisher, 0, len(publishers))
	for _, publisher := range publishers {
		if publisher != nil {
			filtered = append(filtered, publisher)
		}
	}
	return &MultiPublisher{publishers: filtered}
}

func (p *MultiPublisher) Publish(ctx context.Context, data []Datum) error {
	var combined error
	for _, publisher := range p.publishers {
		combined = errors.Join(combined, publisher.Publish(ctx, data))
	}
	return combined
}

func (p *MultiPublisher) Close(ctx context.Context) error {
	var combined error
	for _, publisher := range p.publishers {
		combined = errors.Join(combined, publisher.Close(ctx))
	}
	return combined
}

func (p *AsyncPublisher) run() {
	defer close(p.done)

	ticker := time.NewTicker(p.flushInterval)
	defer ticker.Stop()

	batch := make([]Datum, 0, p.maxBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := p.client.Put(context.Background(), batch); err != nil && p.reporter != nil {
			p.reporter("failed to publish CloudWatch metrics: %v", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case datum := <-p.queue:
			batch = append(batch, datum)
			if len(batch) >= p.maxBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-p.stop:
			for {
				select {
				case datum := <-p.queue:
					batch = append(batch, datum)
					if len(batch) >= p.maxBatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (p *AsyncPublisher) noteDrop() {
	dropped := p.dropped.Add(1)
	if p.reporter != nil && dropped%100 == 1 {
		p.reporter("dropping CloudWatch metrics because the queue is full (total dropped: %d)", dropped)
	}
}

func cloneDatum(datum Datum) Datum {
	cloned := datum
	if datum.Dimensions != nil {
		cloned.Dimensions = make(map[string]string, len(datum.Dimensions))
		for key, value := range datum.Dimensions {
			cloned.Dimensions[key] = value
		}
	}
	return cloned
}
