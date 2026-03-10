package logging

import (
	"context"
	"errors"
	"io"
	"sync"
)

type Sink interface {
	Log(context.Context, Entry) error
	Close(context.Context) error
}

type StdoutSink struct {
	mu sync.Mutex
	w  io.Writer
}

func NewStdoutSink(writer io.Writer) *StdoutSink {
	return &StdoutSink{w: writer}
}

func (s *StdoutSink) Log(_ context.Context, entry Entry) error {
	body, err := Marshal(entry)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.w.Write(body); err != nil {
		return err
	}
	_, err = s.w.Write([]byte("\n"))
	return err
}

func (s *StdoutSink) Close(context.Context) error {
	return nil
}

type MultiSink struct {
	sinks []Sink
}

type NopSink struct{}

func NewMultiSink(sinks ...Sink) *MultiSink {
	filtered := make([]Sink, 0, len(sinks))
	for _, sink := range sinks {
		if sink != nil {
			filtered = append(filtered, sink)
		}
	}
	return &MultiSink{sinks: filtered}
}

func (s *MultiSink) Log(ctx context.Context, entry Entry) error {
	var combined error
	for _, sink := range s.sinks {
		combined = errors.Join(combined, sink.Log(ctx, entry))
	}
	return combined
}

func (s *MultiSink) Close(ctx context.Context) error {
	var combined error
	for _, sink := range s.sinks {
		combined = errors.Join(combined, sink.Close(ctx))
	}
	return combined
}

func (NopSink) Log(context.Context, Entry) error {
	return nil
}

func (NopSink) Close(context.Context) error {
	return nil
}
