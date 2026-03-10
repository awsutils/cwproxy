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
)

const (
	DefaultProxyPort      = 8081
	DefaultAppPort        = 8080
	DefaultHealthPath     = "/health"
	DefaultHealthInterval = 30 * time.Second
	DefaultCaptureLimit   = 64 << 10
)

type Config struct {
	ProxyPort          int
	AppPort            int
	AppName            string
	LogGroupName       string
	HealthLogGroupName string
	TargetURL          *url.URL
	HealthURLs         []*url.URL
	HealthInterval     time.Duration
	CaptureBodyLimit   int
}

type lookupEnvFunc func(string) (string, bool)
type hostnameFunc func() (string, error)

func Load() (Config, error) {
	return LoadFromEnv(os.LookupEnv, os.Hostname)
}

func LoadFromEnv(lookupEnv lookupEnvFunc, hostname hostnameFunc) (Config, error) {
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
	healthURLs, err := ResolveHealthURLs(rawHealthURLs, appPort)
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

	return Config{
		ProxyPort:          proxyPort,
		AppPort:            appPort,
		AppName:            appName,
		LogGroupName:       logGroupName,
		HealthLogGroupName: healthLogGroupName,
		TargetURL: &url.URL{
			Scheme: "http",
			Host:   net.JoinHostPort("127.0.0.1", strconv.Itoa(appPort)),
		},
		HealthURLs:       healthURLs,
		HealthInterval:   DefaultHealthInterval,
		CaptureBodyLimit: DefaultCaptureLimit,
	}, nil
}

func ResolveHealthURLs(raw string, appPort int) ([]*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = fmt.Sprintf("127.0.0.1:%d%s", appPort, DefaultHealthPath)
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
			host = "127.0.0.1"
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

func envOrDefault(lookupEnv lookupEnvFunc, key, fallback string) string {
	if value, ok := lookupEnv(key); ok {
		return value
	}
	return fallback
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
