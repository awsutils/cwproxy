package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/awsutils/cwproxy/internal/proxy"
)

const (
	DefaultProxyPort      = 8081
	DefaultAppHost        = "127.0.0.1"
	DefaultAppPort        = 8080
	DefaultHealthPath     = "/health"
	DefaultHealthInterval = 30 * time.Second
	DefaultCaptureLimit   = 64 << 10
	DefaultRetryStatuses  = "500-599"
)

type Config struct {
	ProxyPort          int
	AppHost            string
	AppPort            int
	AppName            string
	LogGroupName       string
	HealthLogGroupName string
	TargetURL          *url.URL
	HealthURLs         []*url.URL
	HealthInterval     time.Duration
	CaptureBodyLimit   int
	RetryPolicy        proxy.RetryPolicy
}

type lookupEnvFunc func(string) (string, bool)
type hostnameFunc func() (string, error)

func Load() (Config, error) {
	return LoadFromEnv(os.LookupEnv, os.Hostname)
}

func LoadFromEnv(lookupEnv lookupEnvFunc, hostname hostnameFunc) (Config, error) {
	appHost, err := parseHostEnv(lookupEnv, "APP_HOST", DefaultAppHost)
	if err != nil {
		return Config{}, err
	}

	appPort, err := parsePortEnv(lookupEnv, "APP_PORT", DefaultAppPort)
	if err != nil {
		return Config{}, err
	}

	proxyPort, err := parsePortEnv(lookupEnv, "PROXY_PORT", DefaultProxyPort)
	if err != nil {
		return Config{}, err
	}

	appName := strings.TrimSpace(envOrDefault(lookupEnv, "APP_NAME", ""))
	if appName == "" {
		appName, err = hostname()
		if err != nil || strings.TrimSpace(appName) == "" {
			appName = "cwproxy"
		}
	}

	rawHealthURLs := strings.TrimSpace(envOrDefault(lookupEnv, "HEALTH_URLS", ""))
	healthURLs, err := ResolveHealthURLs(rawHealthURLs, appHost, appPort)
	if err != nil {
		return Config{}, err
	}

	logGroupName := strings.TrimSpace(envOrDefault(lookupEnv, "LOG_GROUP_NAME", fmt.Sprintf("/app/log/%s", appName)))
	if logGroupName == "" {
		logGroupName = fmt.Sprintf("/app/log/%s", appName)
	}
	healthLogGroupName := strings.TrimSpace(envOrDefault(lookupEnv, "HEALTH_LOG_GROUP_NAME", fmt.Sprintf("/app/log/%s/health", appName)))
	if healthLogGroupName == "" {
		healthLogGroupName = fmt.Sprintf("/app/log/%s/health", appName)
	}
	retryPolicy, err := loadRetryPolicy(lookupEnv)
	if err != nil {
		return Config{}, err
	}

	return Config{
		ProxyPort:          proxyPort,
		AppHost:            appHost,
		AppPort:            appPort,
		AppName:            appName,
		LogGroupName:       logGroupName,
		HealthLogGroupName: healthLogGroupName,
		TargetURL: &url.URL{
			Scheme: "http",
			Host:   net.JoinHostPort(appHost, strconv.Itoa(appPort)),
		},
		HealthURLs:       healthURLs,
		HealthInterval:   DefaultHealthInterval,
		CaptureBodyLimit: DefaultCaptureLimit,
		RetryPolicy:      retryPolicy,
	}, nil
}

func ResolveHealthURLs(raw, appHost string, appPort int) ([]*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = net.JoinHostPort(appHost, strconv.Itoa(appPort)) + DefaultHealthPath
	}

	parts := strings.Split(raw, ",")
	partials := make([]partialHealthURL, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			return nil, errors.New("HEALTH_URLS contains an empty entry")
		}
		parsed, err := parsePartialHealthURL(trimmed)
		if err != nil {
			return nil, fmt.Errorf("parse HEALTH_URLS entry %q: %w", trimmed, err)
		}
		partials = append(partials, parsed)
	}

	basePath := DefaultHealthPath
	if partials[0].Path != "" {
		basePath = partials[0].Path
	}

	resolved := make([]*url.URL, 0, len(partials))
	for index, partial := range partials {
		scheme := partial.Scheme
		if scheme == "" {
			scheme = "http"
		}
		scheme = strings.ToLower(scheme)
		if scheme != "http" && scheme != "https" {
			return nil, fmt.Errorf("unsupported scheme %q", scheme)
		}

		host := partial.Host
		if host == "" {
			host = appHost
		}

		port := partial.Port
		if port == 0 {
			switch {
			case scheme == "https":
				port = 443
			case index == 0:
				port = appPort
			default:
				port = 80
			}
		}

		path := partial.Path
		if path == "" {
			path = basePath
		}
		if path == "" {
			path = DefaultHealthPath
		}

		resolved = append(resolved, &url.URL{
			Scheme:   scheme,
			Host:     net.JoinHostPort(host, strconv.Itoa(port)),
			Path:     path,
			RawQuery: partial.RawQuery,
		})
	}

	return resolved, nil
}

func parsePortEnv(lookupEnv lookupEnvFunc, key string, fallback int) (int, error) {
	raw := strings.TrimSpace(envOrDefault(lookupEnv, key, ""))
	if raw == "" {
		return fallback, nil
	}

	port, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid integer port: %w", key, err)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s must be between 1 and 65535", key)
	}
	return port, nil
}

func parseHostEnv(lookupEnv lookupEnvFunc, key, fallback string) (string, error) {
	raw := strings.TrimSpace(envOrDefault(lookupEnv, key, fallback))
	if raw == "" {
		raw = fallback
	}
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		raw = strings.TrimPrefix(strings.TrimSuffix(raw, "]"), "[")
	}
	if strings.Contains(raw, "://") || strings.ContainsAny(raw, "/?#@") {
		return "", fmt.Errorf("%s must be a bare host without scheme, path, query, or user info", key)
	}
	if _, _, err := net.SplitHostPort(raw); err == nil {
		return "", fmt.Errorf("%s must not include a port", key)
	}
	return raw, nil
}

func envOrDefault(lookupEnv lookupEnvFunc, key, fallback string) string {
	if value, ok := lookupEnv(key); ok {
		return value
	}
	return fallback
}

func loadRetryPolicy(lookupEnv lookupEnvFunc) (proxy.RetryPolicy, error) {
	maxAttempts, err := parsePositiveIntEnv(lookupEnv, "BACKEND_RETRY_MAX_ATTEMPTS", proxy.DefaultRetryMaxAttempts, 1)
	if err != nil {
		return proxy.RetryPolicy{}, err
	}
	initialBackoff, err := parseDurationEnv(lookupEnv, "BACKEND_RETRY_INITIAL_BACKOFF", proxy.DefaultRetryInitialBackoff)
	if err != nil {
		return proxy.RetryPolicy{}, err
	}
	maxBackoff, err := parseDurationEnv(lookupEnv, "BACKEND_RETRY_MAX_BACKOFF", proxy.DefaultRetryMaxBackoff)
	if err != nil {
		return proxy.RetryPolicy{}, err
	}
	if maxBackoff < initialBackoff {
		return proxy.RetryPolicy{}, errors.New("BACKEND_RETRY_MAX_BACKOFF must be greater than or equal to BACKEND_RETRY_INITIAL_BACKOFF")
	}
	backoffMultiplier, err := parseFloatEnv(lookupEnv, "BACKEND_RETRY_BACKOFF_MULTIPLIER", proxy.DefaultRetryBackoffMultiplier, 1)
	if err != nil {
		return proxy.RetryPolicy{}, err
	}
	methods := parseCSVEnv(lookupEnv, "BACKEND_RETRY_METHODS", proxy.DefaultRetryMethods)
	statusCodes, err := parseStatusCodesEnv(lookupEnv, "BACKEND_RETRY_STATUS_CODES", DefaultRetryStatuses)
	if err != nil {
		return proxy.RetryPolicy{}, err
	}
	retryOnTransportErrors, err := parseBoolEnv(lookupEnv, "BACKEND_RETRY_ON_TRANSPORT_ERRORS", false)
	if err != nil {
		return proxy.RetryPolicy{}, err
	}
	bodyBufferBytes, err := parsePositiveInt64Env(lookupEnv, "BACKEND_RETRY_BODY_BUFFER_BYTES", proxy.DefaultRetryBodyBufferBytes, 0)
	if err != nil {
		return proxy.RetryPolicy{}, err
	}

	return proxy.RetryPolicy{
		MaxAttempts:            maxAttempts,
		InitialBackoff:         initialBackoff,
		MaxBackoff:             maxBackoff,
		BackoffMultiplier:      backoffMultiplier,
		Methods:                methods,
		StatusCodes:            statusCodes,
		RetryOnTransportErrors: retryOnTransportErrors,
		BodyBufferBytes:        bodyBufferBytes,
	}, nil
}

type partialHealthURL struct {
	Scheme   string
	Host     string
	Port     int
	Path     string
	RawQuery string
}

func parsePartialHealthURL(raw string) (partialHealthURL, error) {
	if strings.Contains(raw, "://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return partialHealthURL{}, err
		}
		port, err := parsePort(parsed.Port())
		if err != nil {
			return partialHealthURL{}, err
		}
		return partialHealthURL{
			Scheme:   parsed.Scheme,
			Host:     parsed.Hostname(),
			Port:     port,
			Path:     parsed.Path,
			RawQuery: parsed.RawQuery,
		}, nil
	}

	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "?") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return partialHealthURL{}, err
		}
		return partialHealthURL{
			Path:     parsed.Path,
			RawQuery: parsed.RawQuery,
		}, nil
	}

	if strings.HasPrefix(raw, ":") {
		return parsePortOnlyHealthURL(raw)
	}

	parsed, err := url.Parse("//" + raw)
	if err != nil {
		return partialHealthURL{}, err
	}
	port, err := parsePort(parsed.Port())
	if err != nil {
		return partialHealthURL{}, err
	}
	return partialHealthURL{
		Host:     parsed.Hostname(),
		Port:     port,
		Path:     parsed.Path,
		RawQuery: parsed.RawQuery,
	}, nil
}

func parsePortOnlyHealthURL(raw string) (partialHealthURL, error) {
	withoutPrefix := raw[1:]
	portText := withoutPrefix
	suffix := ""

	if index := strings.IndexAny(withoutPrefix, "/?"); index >= 0 {
		portText = withoutPrefix[:index]
		suffix = withoutPrefix[index:]
	}

	port, err := parsePort(portText)
	if err != nil {
		return partialHealthURL{}, err
	}

	partial := partialHealthURL{Port: port}
	if suffix == "" {
		return partial, nil
	}

	parsed, err := url.Parse(suffix)
	if err != nil {
		return partialHealthURL{}, err
	}
	partial.Path = parsed.Path
	partial.RawQuery = parsed.RawQuery
	return partial, nil
}

func parsePort(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid port %q: %w", raw, err)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid port %q", raw)
	}
	return port, nil
}

func parseDurationEnv(lookupEnv lookupEnvFunc, key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(envOrDefault(lookupEnv, key, ""))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid duration: %w", key, err)
	}
	if value < 0 {
		return 0, fmt.Errorf("%s must not be negative", key)
	}
	return value, nil
}

func parseFloatEnv(lookupEnv lookupEnvFunc, key string, fallback float64, minimum float64) (float64, error) {
	raw := strings.TrimSpace(envOrDefault(lookupEnv, key, ""))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid number: %w", key, err)
	}
	if value < minimum {
		return 0, fmt.Errorf("%s must be greater than or equal to %g", key, minimum)
	}
	return value, nil
}

func parsePositiveIntEnv(lookupEnv lookupEnvFunc, key string, fallback int, minimum int) (int, error) {
	raw := strings.TrimSpace(envOrDefault(lookupEnv, key, ""))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid integer: %w", key, err)
	}
	if value < minimum {
		return 0, fmt.Errorf("%s must be greater than or equal to %d", key, minimum)
	}
	return value, nil
}

func parsePositiveInt64Env(lookupEnv lookupEnvFunc, key string, fallback int64, minimum int64) (int64, error) {
	raw := strings.TrimSpace(envOrDefault(lookupEnv, key, ""))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid integer: %w", key, err)
	}
	if value < minimum {
		return 0, fmt.Errorf("%s must be greater than or equal to %d", key, minimum)
	}
	return value, nil
}

func parseBoolEnv(lookupEnv lookupEnvFunc, key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(envOrDefault(lookupEnv, key, ""))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a valid boolean: %w", key, err)
	}
	return value, nil
}

func parseCSVEnv(lookupEnv lookupEnvFunc, key string, fallback []string) []string {
	raw := strings.TrimSpace(envOrDefault(lookupEnv, key, ""))
	if raw == "" {
		return append([]string(nil), fallback...)
	}
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		values = append(values, trimmed)
	}
	return values
}

func parseStatusCodesEnv(lookupEnv lookupEnvFunc, key, fallback string) ([]int, error) {
	raw := strings.TrimSpace(envOrDefault(lookupEnv, key, fallback))
	if raw == "" {
		return nil, nil
	}

	parts := strings.Split(raw, ",")
	statusCodes := make([]int, 0, len(parts))
	for _, part := range parts {
		codes, err := proxy.ParseStatusCodes(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("%s contains invalid status code token %q: %w", key, strings.TrimSpace(part), err)
		}
		statusCodes = append(statusCodes, codes...)
	}
	return statusCodes, nil
}
