package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultRetryMaxAttempts       = 2
	DefaultRetryInitialBackoff    = 50 * time.Millisecond
	DefaultRetryMaxBackoff        = 250 * time.Millisecond
	DefaultRetryBackoffMultiplier = 2.0
	DefaultRetryBodyBufferBytes   = 64 << 10
)

var DefaultRetryMethods = []string{
	http.MethodGet,
	http.MethodHead,
	http.MethodOptions,
}

type RetryPolicy struct {
	MaxAttempts            int
	InitialBackoff         time.Duration
	MaxBackoff             time.Duration
	BackoffMultiplier      float64
	Methods                []string
	StatusCodes            []int
	RetryOnTransportErrors bool
	BodyBufferBytes        int64
}

func (p RetryPolicy) normalize() RetryPolicy {
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	if p.InitialBackoff < 0 {
		p.InitialBackoff = 0
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = p.InitialBackoff
	}
	if p.MaxBackoff < p.InitialBackoff {
		p.MaxBackoff = p.InitialBackoff
	}
	if p.BackoffMultiplier < 1 {
		p.BackoffMultiplier = 1
	}
	if p.BodyBufferBytes < 0 {
		p.BodyBufferBytes = 0
	}
	p.Methods = normalizeMethods(p.Methods)
	p.StatusCodes = normalizeStatusCodes(p.StatusCodes)
	return p
}

func (p RetryPolicy) enabled() bool {
	p = p.normalize()
	return p.MaxAttempts > 1 && len(p.Methods) > 0 && (len(p.StatusCodes) > 0 || p.RetryOnTransportErrors)
}

type retryTransport struct {
	next   http.RoundTripper
	policy RetryPolicy
}

func newRetryTransport(next http.RoundTripper, policy RetryPolicy) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}

	policy = policy.normalize()
	if !policy.enabled() {
		return next
	}

	return &retryTransport{
		next:   next,
		policy: policy,
	}
}

func (t *retryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || !t.shouldRetryRequest(request) {
		return t.next.RoundTrip(request)
	}

	replayable, err := prepareRetryableRequestBody(request, t.policy.BodyBufferBytes)
	if err != nil {
		return nil, err
	}

	for attempt := 1; attempt <= t.policy.MaxAttempts; attempt++ {
		current := request
		if attempt > 1 {
			current, err = cloneRequestForRetry(request)
			if err != nil {
				return nil, err
			}
		}

		response, roundTripErr := t.next.RoundTrip(current)
		if roundTripErr != nil {
			if !replayable || !t.policy.RetryOnTransportErrors || !isRetryableTransportError(roundTripErr) || attempt == t.policy.MaxAttempts {
				return nil, roundTripErr
			}
			if err := waitForRetry(request.Context(), t.policy.retryBackoff(attempt)); err != nil {
				return nil, err
			}
			continue
		}

		if !replayable || !t.shouldRetryStatus(response.StatusCode) || attempt == t.policy.MaxAttempts {
			return response, nil
		}

		if err := waitForRetry(request.Context(), t.policy.retryBackoff(attempt)); err != nil {
			return response, nil
		}
		closeResponseBody(response.Body)
	}

	return t.next.RoundTrip(request)
}

func (t *retryTransport) shouldRetryRequest(request *http.Request) bool {
	if request == nil {
		return false
	}
	if request.Method == http.MethodConnect || isUpgradeRequest(request) {
		return false
	}
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	return slices.Contains(t.policy.Methods, method)
}

func (t *retryTransport) shouldRetryStatus(status int) bool {
	return slices.Contains(t.policy.StatusCodes, status)
}

func (p RetryPolicy) retryBackoff(attempt int) time.Duration {
	if attempt < 1 || p.InitialBackoff <= 0 {
		return 0
	}

	delay := float64(p.InitialBackoff)
	if attempt > 1 {
		delay *= math.Pow(p.BackoffMultiplier, float64(attempt-1))
	}
	if delay > float64(p.MaxBackoff) {
		delay = float64(p.MaxBackoff)
	}
	if delay < 0 {
		return 0
	}
	return time.Duration(delay)
}

func prepareRetryableRequestBody(request *http.Request, limit int64) (bool, error) {
	if request == nil || request.Body == nil || request.Body == http.NoBody {
		return true, nil
	}
	if request.GetBody != nil {
		return true, nil
	}
	if limit <= 0 {
		return false, nil
	}
	if request.ContentLength > limit && request.ContentLength >= 0 {
		return false, nil
	}

	bufferLimit := limit + 1
	if limit >= math.MaxInt64-1 {
		bufferLimit = math.MaxInt64
	}

	payload, err := io.ReadAll(io.LimitReader(request.Body, bufferLimit))
	if err != nil {
		return false, err
	}
	if int64(len(payload)) > limit {
		request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(payload), request.Body))
		return false, nil
	}

	setReplayableRequestBody(request, payload)
	return true, nil
}

func cloneRequestForRetry(request *http.Request) (*http.Request, error) {
	if request == nil {
		return nil, errors.New("request is required")
	}

	cloned := request.Clone(request.Context())
	switch {
	case request.Body == nil || request.Body == http.NoBody:
		cloned.Body = request.Body
	case request.GetBody != nil:
		body, err := request.GetBody()
		if err != nil {
			return nil, err
		}
		cloned.Body = body
	default:
		return nil, errors.New("request body is not replayable")
	}
	return cloned, nil
}

func setReplayableRequestBody(request *http.Request, payload []byte) {
	if request == nil {
		return
	}

	if len(payload) == 0 {
		request.Body = http.NoBody
		request.GetBody = func() (io.ReadCloser, error) {
			return http.NoBody, nil
		}
		request.ContentLength = 0
		return
	}

	clone := append([]byte(nil), payload...)
	request.Body = io.NopCloser(bytes.NewReader(clone))
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(clone)), nil
	}
	request.ContentLength = int64(len(clone))
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isRetryableTransportError(err error) bool {
	if err == nil {
		return false
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func isUpgradeRequest(request *http.Request) bool {
	if request == nil {
		return false
	}
	if strings.TrimSpace(request.Header.Get("Upgrade")) == "" {
		return false
	}
	for _, value := range request.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

func closeResponseBody(body io.ReadCloser) {
	if body == nil {
		return
	}
	_ = body.Close()
}

func normalizeMethods(methods []string) []string {
	if len(methods) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(methods))
	normalized := make([]string, 0, len(methods))
	for _, method := range methods {
		trimmed := strings.ToUpper(strings.TrimSpace(method))
		if trimmed == "" {
			continue
		}
		if _, found := seen[trimmed]; found {
			continue
		}
		seen[trimmed] = struct{}{}
		normalized = append(normalized, trimmed)
	}
	slices.Sort(normalized)
	return normalized
}

func normalizeStatusCodes(statusCodes []int) []int {
	if len(statusCodes) == 0 {
		return nil
	}

	seen := make(map[int]struct{}, len(statusCodes))
	normalized := make([]int, 0, len(statusCodes))
	for _, statusCode := range statusCodes {
		if statusCode < 100 || statusCode > 599 {
			continue
		}
		if _, found := seen[statusCode]; found {
			continue
		}
		seen[statusCode] = struct{}{}
		normalized = append(normalized, statusCode)
	}
	slices.Sort(normalized)
	return normalized
}

func statusCodeListFromRange(start, end int) []int {
	if start > end {
		start, end = end, start
	}
	statusCodes := make([]int, 0, end-start+1)
	for statusCode := start; statusCode <= end; statusCode++ {
		statusCodes = append(statusCodes, statusCode)
	}
	return statusCodes
}

func parseStatusCodeToken(token string) ([]int, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("status code token is empty")
	}
	if strings.Contains(token, "-") {
		parts := strings.SplitN(token, "-", 2)
		if len(parts) != 2 {
			return nil, errors.New("invalid status code range")
		}
		start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			return nil, err
		}
		end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, err
		}
		if start < 100 || start > 599 || end < 100 || end > 599 {
			return nil, errors.New("status code range must stay within 100-599")
		}
		return statusCodeListFromRange(start, end), nil
	}

	statusCode, err := strconv.Atoi(token)
	if err != nil {
		return nil, err
	}
	if statusCode < 100 || statusCode > 599 {
		return nil, errors.New("status code must stay within 100-599")
	}
	return []int{statusCode}, nil
}

func ParseStatusCodes(token string) ([]int, error) {
	return parseStatusCodeToken(token)
}
