package cwlogs

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/awsutils/cwproxy/internal/logging"
	"github.com/awsutils/cwproxy/internal/metrics"
)

type StdoutSink struct {
	mu sync.Mutex
	w  io.Writer
}

func NewStdoutSink(writer io.Writer) *StdoutSink {
	if writer == nil {
		writer = io.Discard
	}
	return &StdoutSink{w: writer}
}

func (s *StdoutSink) Log(_ context.Context, entry logging.Entry) error {
	body, err := logging.Marshal(entry)
	if err != nil {
		return err
	}
	return s.write(body)
}

func (s *StdoutSink) LogWithMetrics(_ context.Context, entry logging.Entry, data []metrics.Datum) error {
	return s.logWithMetrics(entry, data, "app/traffic")
}

func (s *StdoutSink) LogHealthWithMetrics(_ context.Context, entry logging.Entry, data []metrics.Datum) error {
	return s.logWithMetrics(entry, data, "app/health")
}

func (s *StdoutSink) Close(context.Context) error {
	return nil
}

func (s *StdoutSink) logWithMetrics(entry logging.Entry, data []metrics.Datum, namespace string) error {
	if len(data) == 0 || namespace == "" {
		return s.Log(context.Background(), entry)
	}

	batches, err := partitionDatums(data, reservedLogRootKeys)
	if err != nil {
		return err
	}
	if len(batches) == 0 {
		return s.Log(context.Background(), entry)
	}

	message, err := marshalStdoutLogWithMetrics(entry, namespace, batches[0])
	if err != nil {
		return err
	}

	combined := s.write(message)
	timestamp := entryTimestamp(entry)
	for _, batch := range batches[1:] {
		metricMessage, err := marshalMetricEvent(entry.AppName, entry.Type, namespace, timestamp, batch, false)
		if err != nil {
			combined = errors.Join(combined, err)
			continue
		}
		combined = errors.Join(combined, s.write(metricMessage))
	}
	return combined
}

func (s *StdoutSink) write(body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.w.Write(body); err != nil {
		return err
	}
	_, err := s.w.Write([]byte("\n"))
	return err
}

func marshalStdoutLogWithMetrics(entry logging.Entry, namespace string, data []metrics.Datum) ([]byte, error) {
	base, err := logging.Marshal(entry)
	if err != nil {
		return nil, err
	}

	fields, envelope, err := buildEMF(namespace, entryTimestamp(entry), data, reservedLogRootKeys)
	if err != nil {
		return nil, err
	}

	return appendEMF(base, fields, envelope)
}
