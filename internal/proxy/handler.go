package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/awsutils/cwproxy/internal/logging"
	"github.com/awsutils/cwproxy/internal/metrics"
)

type Options struct {
	AppName         string
	MaxCaptureBytes int
	Reporter        func(string, ...any)
	Now             func() time.Time
	Transport       http.RoundTripper
}

type Handler struct {
	appName         string
	targetURL       *url.URL
	sink            logging.Sink
	publisher       metrics.Publisher
	reporter        func(string, ...any)
	maxCaptureBytes int
	now             func() time.Time
	proxy           *httputil.ReverseProxy
}

type stateKey struct{}

type exchangeState struct {
	start           time.Time
	requestCapture  *captureBuffer
	responseCapture *captureBuffer
	responseHeaders http.Header
	responseCookies []*http.Cookie
	responseStatus  int
}

func New(targetURL *url.URL, sink logging.Sink, publisher metrics.Publisher, options Options) *Handler {
	now := options.Now
	if now == nil {
		now = time.Now
	}

	transport := options.Transport
	if transport == nil {
		transport = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				KeepAlive: 30 * time.Second,
				Timeout:   5 * time.Second,
			}).DialContext,
			DisableCompression:    false,
			ForceAttemptHTTP2:     true,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   256,
			ResponseHeaderTimeout: 30 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}

	reporter := options.Reporter
	errorLogger := log.New(io.Discard, "", 0)

	handler := &Handler{
		appName:         options.AppName,
		targetURL:       cloneURL(targetURL),
		sink:            sink,
		publisher:       publisher,
		reporter:        reporter,
		maxCaptureBytes: options.MaxCaptureBytes,
		now:             now,
	}
	if handler.sink == nil {
		handler.sink = logging.NopSink{}
	}
	if handler.publisher == nil {
		handler.publisher = metrics.NopPublisher{}
	}

	handler.proxy = &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(handler.targetURL)
			request.SetXForwarded()
			request.Out.Host = request.In.Host
		},
		Transport:  transport,
		BufferPool: newBufferPool(32 << 10),
		ErrorLog:   errorLogger,
		ModifyResponse: func(response *http.Response) error {
			state := getExchangeState(response.Request.Context())
			if state == nil {
				return nil
			}
			state.responseStatus = response.StatusCode
			state.responseHeaders = response.Header.Clone()
			state.responseCookies = response.Cookies()
			state.responseCapture = newCaptureBuffer(handler.maxCaptureBytes)
			response.Body = newTeeReadCloser(response.Body, state.responseCapture)
			return nil
		},
		ErrorHandler: func(writer http.ResponseWriter, request *http.Request, err error) {
			if reporter != nil {
				reporter("reverse proxy upstream error: %v", err)
			}
			http.Error(writer, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
			state := getExchangeState(request.Context())
			if state != nil {
				state.responseStatus = http.StatusBadGateway
			}
		},
	}

	return handler
}

func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	recorder := newResponseRecorder(writer)
	state := &exchangeState{
		start:          h.now(),
		requestCapture: newCaptureBuffer(h.maxCaptureBytes),
	}

	requestWithState := request.Clone(context.WithValue(request.Context(), stateKey{}, state))
	requestWithState.Body = newTeeReadCloser(request.Body, state.requestCapture)

	defer func() {
		if recovered := recover(); recovered != nil {
			if h.reporter != nil {
				h.reporter("reverse proxy panic recovered: %v", recovered)
			}
			state.responseStatus = http.StatusInternalServerError
			http.Error(recorder, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		h.finalize(requestWithState, recorder, state)
	}()

	h.proxy.ServeHTTP(recorder, requestWithState)
}

func (h *Handler) finalize(request *http.Request, recorder *responseRecorder, state *exchangeState) {
	end := h.now()
	status := state.responseStatus
	if status == 0 {
		status = recorder.Status()
	}

	host, port := splitHostPort(request.Host, request.TLS != nil)
	path := request.URL.Path
	if path == "" {
		path = "/"
	}

	requestURI := path
	if request.URL.RawQuery != "" {
		requestURI += "?" + request.URL.RawQuery
	}

	requestBody, requestBodyTruncated := state.requestCapture.Snapshot()
	responseBody := []byte(nil)
	responseBodyTruncated := false
	if state.responseCapture != nil {
		responseBody, responseBodyTruncated = state.responseCapture.Snapshot()
	}

	responseHeaders := state.responseHeaders
	if responseHeaders == nil {
		responseHeaders = recorder.Header().Clone()
	}

	entry := logging.NewEntry(
		h.appName,
		logging.Request{
			Time:    state.start.UnixMilli(),
			Host:    host,
			Port:    port,
			Path:    path,
			Method:  request.Method,
			URL:     net.JoinHostPort(host, strconv.Itoa(port)) + requestURI,
			Queries: logging.NormalizeValues(request.URL.Query()),
			Cookies: logging.NormalizeCookies(request.Cookies()),
			Headers: logging.NormalizeHeaders(request.Header),
			Body:    logging.ParseBody(request.Header.Get("Content-Type"), requestBody, requestBodyTruncated),
		},
		logging.Response{
			Time:       end.UnixMilli(),
			Status:     status,
			Headers:    logging.NormalizeHeaders(responseHeaders, "Set-Cookie"),
			SetCookies: logging.NormalizeCookies(state.responseCookies),
			Body:       logging.ParseBody(responseHeaders.Get("Content-Type"), responseBody, responseBodyTruncated),
		},
		end.Sub(state.start),
	)

	responseSize := recorder.BytesWritten()
	if state.responseCapture != nil {
		responseSize = state.responseCapture.Total()
	}

	metricData := buildMetrics(h.appName, path, request.Method, status, end.Sub(state.start), state.requestCapture.Total(), responseSize)
	if metricSink, ok := h.sink.(logging.MetricSink); ok {
		if err := metricSink.LogWithMetrics(context.Background(), entry, metricData); err != nil && h.reporter != nil {
			h.reporter("failed to write CloudWatch EMF log entry: %v", err)
		}
		return
	}

	if err := h.sink.Log(context.Background(), entry); err != nil && h.reporter != nil {
		h.reporter("failed to write log entry: %v", err)
	}
	if err := h.publisher.Publish(context.Background(), metricData); err != nil && h.reporter != nil {
		h.reporter("failed to publish proxy metrics: %v", err)
	}
}

func buildMetrics(appName, path, method string, status int, latency time.Duration, requestBytes, responseBytes int64) []metrics.Datum {
	aggregateDimensions := map[string]string{
		"AppName": appName,
	}
	requestDimensions := map[string]string{
		"AppName":  appName,
		"Endpoint": path,
		"Method":   method,
	}

	data := make([]metrics.Datum, 0, 10)
	data = append(data, buildTrafficMetricSet(aggregateDimensions, latency, requestBytes, responseBytes)...)
	data = append(data, buildTrafficMetricSet(requestDimensions, latency, requestBytes, responseBytes)...)

	if statusMetric := statusBucketMetricName(status); statusMetric != "" {
		data = append(data,
			metrics.Datum{
				Name:       statusMetric,
				Value:      1,
				Unit:       metrics.UnitCount,
				Dimensions: aggregateDimensions,
			},
			metrics.Datum{
				Name:       statusMetric,
				Value:      1,
				Unit:       metrics.UnitCount,
				Dimensions: requestDimensions,
			},
		)
	}

	return data
}

func buildTrafficMetricSet(dimensions map[string]string, latency time.Duration, requestBytes, responseBytes int64) []metrics.Datum {
	return []metrics.Datum{
		{
			Name:       "RequestCount",
			Value:      1,
			Unit:       metrics.UnitCount,
			Dimensions: cloneDimensions(dimensions),
		},
		{
			Name:       "Latency",
			Value:      float64(latency) / float64(time.Millisecond),
			Unit:       metrics.UnitMilliseconds,
			Dimensions: cloneDimensions(dimensions),
		},
		{
			Name:       "RequestBodySize",
			Value:      float64(requestBytes),
			Unit:       metrics.UnitBytes,
			Dimensions: cloneDimensions(dimensions),
		},
		{
			Name:       "ResponseBodySize",
			Value:      float64(responseBytes),
			Unit:       metrics.UnitBytes,
			Dimensions: cloneDimensions(dimensions),
		},
	}
}

func statusBucketMetricName(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2XXStatusCode"
	case status >= 400 && status < 500:
		return "4XXStatusCode"
	case status >= 500 && status < 600:
		return "5XXStatusCode"
	default:
		return ""
	}
}

func cloneDimensions(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func getExchangeState(ctx context.Context) *exchangeState {
	state, _ := ctx.Value(stateKey{}).(*exchangeState)
	return state
}

func cloneURL(target *url.URL) *url.URL {
	if target == nil {
		return &url.URL{}
	}
	cloned := *target
	return &cloned
}

func splitHostPort(hostport string, tlsEnabled bool) (string, int) {
	if hostport == "" {
		if tlsEnabled {
			return "", 443
		}
		return "", 80
	}

	host, portText, err := net.SplitHostPort(hostport)
	if err == nil {
		port, parseErr := strconv.Atoi(portText)
		if parseErr == nil && port > 0 {
			return host, port
		}
	}

	if strings.HasPrefix(hostport, "[") && strings.HasSuffix(hostport, "]") {
		hostport = strings.TrimPrefix(strings.TrimSuffix(hostport, "]"), "[")
	}

	defaultPort := 80
	if tlsEnabled {
		defaultPort = 443
	}
	return hostport, defaultPort
}

type bufferPool struct {
	pool sync.Pool
	size int
}

func newBufferPool(size int) *bufferPool {
	return &bufferPool{
		size: size,
		pool: sync.Pool{
			New: func() any {
				buffer := make([]byte, size)
				return &buffer
			},
		},
	}
}

func (p *bufferPool) Get() []byte {
	buffer, _ := p.pool.Get().(*[]byte)
	if buffer == nil || cap(*buffer) < p.size {
		return make([]byte, p.size)
	}
	return (*buffer)[:p.size]
}

func (p *bufferPool) Put(buffer []byte) {
	if cap(buffer) < p.size {
		return
	}
	buffer = buffer[:p.size]
	p.pool.Put(&buffer)
}

type responseRecorder struct {
	http.ResponseWriter
	status       int
	wroteHeader  bool
	bytesWritten int64
}

func newResponseRecorder(writer http.ResponseWriter) *responseRecorder {
	return &responseRecorder{ResponseWriter: writer}
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	if !r.wroteHeader {
		r.status = statusCode
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	count, err := r.ResponseWriter.Write(body)
	r.bytesWritten += int64(count)
	return count, err
}

func (r *responseRecorder) Status() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func (r *responseRecorder) BytesWritten() int64 {
	return r.bytesWritten
}

func (r *responseRecorder) Flush() {
	flusher, ok := r.ResponseWriter.(http.Flusher)
	if ok {
		flusher.Flush()
	}
}

func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func (r *responseRecorder) Push(target string, options *http.PushOptions) error {
	pusher, ok := r.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

func (r *responseRecorder) ReadFrom(reader io.Reader) (int64, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if readerFrom, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		count, err := readerFrom.ReadFrom(reader)
		r.bytesWritten += count
		return count, err
	}
	count, err := io.Copy(r.ResponseWriter, reader)
	r.bytesWritten += count
	return count, err
}
