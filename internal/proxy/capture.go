package proxy

import (
	"bytes"
	"io"
)

type captureBuffer struct {
	limit     int
	buf       bytes.Buffer
	total     int64
	truncated bool
}

func newCaptureBuffer(limit int) *captureBuffer {
	if limit <= 0 {
		limit = 64 << 10
	}
	return &captureBuffer{limit: limit}
}

func (c *captureBuffer) Write(body []byte) (int, error) {
	c.total += int64(len(body))

	remaining := c.limit - c.buf.Len()
	if remaining <= 0 {
		c.truncated = true
		return len(body), nil
	}

	chunk := body
	if len(chunk) > remaining {
		chunk = chunk[:remaining]
		c.truncated = true
	}

	if _, err := c.buf.Write(chunk); err != nil {
		return 0, err
	}
	return len(body), nil
}

func (c *captureBuffer) Snapshot() ([]byte, bool) {
	return append([]byte(nil), c.buf.Bytes()...), c.truncated
}

func (c *captureBuffer) Total() int64 {
	return c.total
}

type teeReadCloser struct {
	reader  io.ReadCloser
	capture *captureBuffer
}

func newTeeReadCloser(reader io.ReadCloser, capture *captureBuffer) io.ReadCloser {
	if reader == nil {
		return nil
	}
	return &teeReadCloser{
		reader:  reader,
		capture: capture,
	}
}

func (t *teeReadCloser) Read(body []byte) (int, error) {
	count, err := t.reader.Read(body)
	if count > 0 && t.capture != nil {
		_, _ = t.capture.Write(body[:count])
	}
	return count, err
}

func (t *teeReadCloser) Close() error {
	return t.reader.Close()
}
