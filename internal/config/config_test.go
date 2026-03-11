package config

import (
	"errors"
	"testing"
)

func TestLoadFromEnvDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFromEnv(emptyLookupEnv, func() (string, error) {
		return "proxy-host", nil
	})
	if err != nil {
		t.Fatalf("LoadFromEnv returned error: %v", err)
	}

	if cfg.ProxyPort != DefaultProxyPort {
		t.Fatalf("ProxyPort = %d, want %d", cfg.ProxyPort, DefaultProxyPort)
	}
	if cfg.AppHost != DefaultAppHost {
		t.Fatalf("AppHost = %q, want %q", cfg.AppHost, DefaultAppHost)
	}
	if cfg.AppPort != DefaultAppPort {
		t.Fatalf("AppPort = %d, want %d", cfg.AppPort, DefaultAppPort)
	}
	if cfg.AppName != "proxy-host" {
		t.Fatalf("AppName = %q, want %q", cfg.AppName, "proxy-host")
	}
	if cfg.LogGroupName != "/app/log/proxy-host" {
		t.Fatalf("LogGroupName = %q", cfg.LogGroupName)
	}
	if cfg.HealthLogGroupName != "/app/log/proxy-host/health" {
		t.Fatalf("HealthLogGroupName = %q", cfg.HealthLogGroupName)
	}
	if got := cfg.TargetURL.String(); got != "http://127.0.0.1:8080" {
		t.Fatalf("TargetURL = %q", got)
	}
	if len(cfg.HealthURLs) != 1 || cfg.HealthURLs[0].String() != "http://127.0.0.1:8080/health" {
		t.Fatalf("HealthURLs = %#v", cfg.HealthURLs)
	}
}

func TestLoadFromEnvFallsBackWhenHostnameFails(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFromEnv(emptyLookupEnv, func() (string, error) {
		return "", errors.New("boom")
	})
	if err != nil {
		t.Fatalf("LoadFromEnv returned error: %v", err)
	}
	if cfg.AppName != "cwproxy" {
		t.Fatalf("AppName = %q, want cwproxy", cfg.AppName)
	}
}

func TestResolveHealthURLsExamples(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		appHost string
		raw     string
		want    []string
	}{
		{
			name:    "path only",
			appHost: DefaultAppHost,
			raw:     "/health",
			want:    []string{"http://127.0.0.1:8080/health"},
		},
		{
			name:    "port and path",
			appHost: DefaultAppHost,
			raw:     ":8081/health",
			want:    []string{"http://127.0.0.1:8081/health"},
		},
		{
			name:    "second host path",
			appHost: DefaultAppHost,
			raw:     "/health,some.alb.example.com/healthz",
			want: []string{
				"http://127.0.0.1:8080/health",
				"http://some.alb.example.com:80/healthz",
			},
		},
		{
			name:    "second host inherits path",
			appHost: DefaultAppHost,
			raw:     "/healthz,some.alb.example.com",
			want: []string{
				"http://127.0.0.1:8080/healthz",
				"http://some.alb.example.com:80/healthz",
			},
		},
		{
			name:    "https defaults to 443",
			appHost: DefaultAppHost,
			raw:     "/healthz,https://some.alb.example.com",
			want: []string{
				"http://127.0.0.1:8080/healthz",
				"https://some.alb.example.com:443/healthz",
			},
		},
		{
			name:    "custom app host applies to omitted hosts",
			appHost: "app.internal",
			raw:     "/health,:8081/ready",
			want: []string{
				"http://app.internal:8080/health",
				"http://app.internal:8081/ready",
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			urls, err := ResolveHealthURLs(test.raw, test.appHost, 8080)
			if err != nil {
				t.Fatalf("ResolveHealthURLs returned error: %v", err)
			}
			if len(urls) != len(test.want) {
				t.Fatalf("ResolveHealthURLs length = %d, want %d", len(urls), len(test.want))
			}
			for index := range urls {
				if got := urls[index].String(); got != test.want[index] {
					t.Fatalf("ResolveHealthURLs[%d] = %q, want %q", index, got, test.want[index])
				}
			}
		})
	}
}

func TestLoadFromEnvUsesConfiguredAppHost(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFromEnv(func(key string) (string, bool) {
		switch key {
		case "APP_HOST":
			return "app.internal", true
		default:
			return "", false
		}
	}, func() (string, error) {
		return "proxy-host", nil
	})
	if err != nil {
		t.Fatalf("LoadFromEnv returned error: %v", err)
	}

	if cfg.AppHost != "app.internal" {
		t.Fatalf("AppHost = %q, want %q", cfg.AppHost, "app.internal")
	}
	if got := cfg.TargetURL.String(); got != "http://app.internal:8080" {
		t.Fatalf("TargetURL = %q", got)
	}
	if len(cfg.HealthURLs) != 1 || cfg.HealthURLs[0].String() != "http://app.internal:8080/health" {
		t.Fatalf("HealthURLs = %#v", cfg.HealthURLs)
	}
}

func TestLoadFromEnvNormalizesBracketedIPv6AppHost(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFromEnv(func(key string) (string, bool) {
		if key == "APP_HOST" {
			return "[::1]", true
		}
		return "", false
	}, func() (string, error) {
		return "proxy-host", nil
	})
	if err != nil {
		t.Fatalf("LoadFromEnv returned error: %v", err)
	}

	if cfg.AppHost != "::1" {
		t.Fatalf("AppHost = %q, want %q", cfg.AppHost, "::1")
	}
	if got := cfg.TargetURL.String(); got != "http://[::1]:8080" {
		t.Fatalf("TargetURL = %q", got)
	}
	if len(cfg.HealthURLs) != 1 || cfg.HealthURLs[0].String() != "http://[::1]:8080/health" {
		t.Fatalf("HealthURLs = %#v", cfg.HealthURLs)
	}
}

func TestLoadFromEnvRejectsInvalidPorts(t *testing.T) {
	t.Parallel()

	_, err := LoadFromEnv(func(key string) (string, bool) {
		if key == "PROXY_PORT" {
			return "70000", true
		}
		return "", false
	}, func() (string, error) {
		return "host", nil
	})
	if err == nil {
		t.Fatal("expected an error for invalid PROXY_PORT")
	}
}

func TestLoadFromEnvRejectsAppHostWithPort(t *testing.T) {
	t.Parallel()

	_, err := LoadFromEnv(func(key string) (string, bool) {
		if key == "APP_HOST" {
			return "example.com:8080", true
		}
		return "", false
	}, func() (string, error) {
		return "host", nil
	})
	if err == nil {
		t.Fatal("expected an error for APP_HOST with a port")
	}
}

func TestLoadFromEnvUsesConfiguredHealthLogGroupName(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFromEnv(func(key string) (string, bool) {
		switch key {
		case "APP_NAME":
			return "cwproxy", true
		case "HEALTH_LOG_GROUP_NAME":
			return "/custom/health/log/group", true
		default:
			return "", false
		}
	}, func() (string, error) {
		return "ignored-hostname", nil
	})
	if err != nil {
		t.Fatalf("LoadFromEnv returned error: %v", err)
	}

	if cfg.HealthLogGroupName != "/custom/health/log/group" {
		t.Fatalf("HealthLogGroupName = %q", cfg.HealthLogGroupName)
	}
}

func emptyLookupEnv(string) (string, bool) {
	return "", false
}
