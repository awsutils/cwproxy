package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"

	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/awsutils/cwproxy/internal/aws/cwlogs"
	"github.com/awsutils/cwproxy/internal/config"
	"github.com/awsutils/cwproxy/internal/health"
	"github.com/awsutils/cwproxy/internal/logging"
	"github.com/awsutils/cwproxy/internal/metadata"
	"github.com/awsutils/cwproxy/internal/metrics"
	"github.com/awsutils/cwproxy/internal/proxy"
)

var nonAlphaNumeric = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "cwproxy: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	reporter := log.New(os.Stderr, "cwproxy: ", log.LstdFlags|log.Lmsgprefix)

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	rootContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	metadataContext, metadataCancel := context.WithTimeout(rootContext, 200*time.Millisecond)
	runtimeMetadata := metadata.Load(metadataContext, metadata.Options{
		Reporter: reporter.Printf,
	})
	metadataCancel()

	stdoutSink := logging.NewStdoutSink(os.Stdout)
	logSink := logging.Sink(stdoutSink)
	proxyMetricPublisher := metrics.Publisher(metrics.NopPublisher{})
	healthMetricPublisher := metrics.Publisher(metrics.NopPublisher{})
	var healthSink logging.Sink

	closers := []contextCloser{stdoutSink}

	awsRegion, awsRegionSource := resolveAWSRegion(runtimeMetadata)

	if awsRegion == "" {
		reporter.Printf("AWS region is not configured; CloudWatch Logs and Metrics are disabled")
	} else {
		if awsRegionSource == "runtime metadata" {
			reporter.Printf("AWS region is not configured in the environment; using %q from runtime metadata", awsRegion)
		}

		awsConfig, awsErr := awscfg.LoadDefaultConfig(rootContext, awscfg.WithRegion(awsRegion))
		if awsErr != nil {
			reporter.Printf("failed to load AWS configuration: %v", awsErr)
		} else {
			client := cloudwatchlogs.NewFromConfig(awsConfig)
			now := time.Now()
			trafficLogSink, trafficSinkErr := cwlogs.New(rootContext, client, cfg.LogGroupName, cwlogs.Options{
				AppName:                cfg.AppName,
				TrafficMetricNamespace: "app/traffic",
				StreamName:             sanitizeStreamName(cfg.AppName+"-traffic", now, os.Getpid()),
				Reporter:               reporter.Printf,
			})
			if trafficSinkErr != nil {
				reporter.Printf("failed to initialize CloudWatch traffic sink: %v", trafficSinkErr)
			} else {
				logSink = logging.NewMultiSink(stdoutSink, trafficLogSink)
				proxyMetricPublisher = trafficLogSink
				closers = append(closers, trafficLogSink)
			}

			healthCWSink, healthSinkErr := cwlogs.New(rootContext, client, cfg.HealthLogGroupName, cwlogs.Options{
				AppName:               cfg.AppName,
				HealthMetricNamespace: "app/health",
				StreamName:            sanitizeStreamName(cfg.AppName+"-health", now, os.Getpid()),
				Reporter:              reporter.Printf,
			})
			if healthSinkErr != nil {
				reporter.Printf("failed to initialize CloudWatch health sink: %v", healthSinkErr)
			} else {
				healthMetricPublisher = healthCWSink
				healthSink = healthCWSink
				closers = append(closers, healthCWSink)
			}
		}
	}

	handler := proxy.New(cfg.TargetURL, logSink, proxyMetricPublisher, proxy.Options{
		AppName:         cfg.AppName,
		Metadata:        runtimeMetadata,
		MaxCaptureBytes: cfg.CaptureBodyLimit,
		Reporter:        reporter.Printf,
	})

	server := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.ProxyPort),
		Handler:           handler,
		ErrorLog:          reporter,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ReadHeaderTimeout: 5 * time.Second,
	}

	healthRunner := health.NewRunner(cfg.HealthURLs, healthMetricPublisher, health.Options{
		AppName:         cfg.AppName,
		Metadata:        runtimeMetadata,
		Interval:        cfg.HealthInterval,
		Sink:            healthSink,
		MaxCaptureBytes: cfg.CaptureBodyLimit,
		Reporter:        reporter.Printf,
	})

	go healthRunner.Run(rootContext)

	serverErrors := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				serverErrors <- fmt.Errorf("server panic recovered: %v", recovered)
			}
		}()
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err = <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	case <-rootContext.Done():
		err = nil
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	shutdownErr := server.Shutdown(shutdownContext)
	closeErr := closeAll(context.Background(), closers...)

	if err != nil {
		return err
	}
	if shutdownErr != nil && !errors.Is(shutdownErr, context.Canceled) {
		return shutdownErr
	}
	return closeErr
}

type contextCloser interface {
	Close(context.Context) error
}

func closeAll(ctx context.Context, closers ...contextCloser) error {
	var combined error
	for _, closer := range closers {
		if closer == nil {
			continue
		}
		combined = errors.Join(combined, closer.Close(ctx))
	}
	return combined
}

func sanitizeStreamName(appName string, now time.Time, pid int) string {
	name := nonAlphaNumeric.ReplaceAllString(appName, "-")
	if name == "" {
		name = "cwproxy"
	}
	return fmt.Sprintf("%s-%d-%d", name, pid, now.Unix())
}

func resolveAWSRegion(snapshot *metadata.Snapshot) (string, string) {
	if region := os.Getenv("AWS_REGION"); region != "" {
		return region, "AWS_REGION"
	}
	if region := os.Getenv("AWS_DEFAULT_REGION"); region != "" {
		return region, "AWS_DEFAULT_REGION"
	}
	if region := metadata.InferAWSRegion(snapshot); region != "" {
		return region, "runtime metadata"
	}
	return "", ""
}
