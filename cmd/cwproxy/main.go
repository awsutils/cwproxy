package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/awsutils/cwproxy/internal/aws/cwlogs"
	"github.com/awsutils/cwproxy/internal/config"
	"github.com/awsutils/cwproxy/internal/health"
	"github.com/awsutils/cwproxy/internal/inspector"
	"github.com/awsutils/cwproxy/internal/logging"
	"github.com/awsutils/cwproxy/internal/metadata"
	"github.com/awsutils/cwproxy/internal/metrics"
	"github.com/awsutils/cwproxy/internal/proxy"
)

var nonAlphaNumeric = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func main() {
	if err := run(); err != nil {
		var exitErr *exitCodeError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.code)
		}
		fmt.Fprintf(os.Stderr, "cwproxy: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	reporter := log.New(os.Stderr, "cwproxy: ", log.LstdFlags|log.Lmsgprefix)
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	metadataContext, metadataCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	runtimeMetadata := metadata.Load(metadataContext, metadata.Options{
		Reporter: reporter.Printf,
	})
	metadataCancel()

	lookupEnv := os.LookupEnv
	var err error
	var child *inspector.Child
	inspectorArgs := os.Args[1:]
	if len(inspectorArgs) > 0 {
		child, err = inspector.Start(inspectorArgs[0], inspectorArgs[1:], os.Environ(), os.Stdin, os.Stdout, os.Stderr)
		if err != nil {
			return fmt.Errorf("start inspector target: %w", err)
		}

		if _, found := os.LookupEnv("APP_PORT"); !found {
			detectedPort, detectErr := waitForInspectorPort(context.Background(), child, signals, reporter.Printf)
			if detectErr != nil {
				return cleanupInspectorStartupFailure(child, reporter.Printf, detectErr)
			}
			reporter.Printf("inspector mode detected listen port %d for %q", detectedPort, child.Command())
			lookupEnv = withEnvOverride(lookupEnv, "APP_PORT", strconv.Itoa(detectedPort))
		}
	}

	cfg, err := config.LoadFromEnv(lookupEnv, func() (string, error) {
		return resolveDefaultAppName(runtimeMetadata, os.Hostname)
	})
	if err != nil {
		return cleanupInspectorStartupFailure(child, reporter.Printf, err)
	}

	stdoutSink := cwlogs.NewStdoutSink(os.Stdout)
	logSink := logging.Sink(stdoutSink)
	proxyMetricPublisher := metrics.Publisher(metrics.NopPublisher{})
	healthMetricPublisher := metrics.Publisher(metrics.NopPublisher{})
	var healthSink logging.Sink = stdoutSink

	closers := []contextCloser{stdoutSink}

	awsRegion, awsRegionSource := resolveAWSRegion(runtimeMetadata)

	if awsRegion == "" {
		reporter.Printf("AWS region is not configured; CloudWatch Logs and Metrics are disabled")
	} else {
		if awsRegionSource == "runtime metadata" {
			reporter.Printf("AWS region is not configured in the environment; using %q from runtime metadata", awsRegion)
		}

		awsConfig, awsErr := awscfg.LoadDefaultConfig(context.Background(), awscfg.WithRegion(awsRegion))
		if awsErr != nil {
			reporter.Printf("failed to load AWS configuration: %v", awsErr)
		} else {
			client := cloudwatchlogs.NewFromConfig(awsConfig)
			now := time.Now()
			trafficLogSink, trafficSinkErr := cwlogs.New(context.Background(), client, cfg.LogGroupName, cwlogs.Options{
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

			healthCWSink, healthSinkErr := cwlogs.New(context.Background(), client, cfg.HealthLogGroupName, cwlogs.Options{
				AppName:               cfg.AppName,
				HealthMetricNamespace: "app/health",
				StreamName:            sanitizeStreamName(cfg.AppName+"-health", now, os.Getpid()),
				Reporter:              reporter.Printf,
			})
			if healthSinkErr != nil {
				reporter.Printf("failed to initialize CloudWatch health sink: %v", healthSinkErr)
			} else {
				healthMetricPublisher = healthCWSink
				healthSink = logging.NewMultiSink(stdoutSink, healthCWSink)
				closers = append(closers, healthCWSink)
			}
		}
	}

	handler := proxy.New(cfg.TargetURL, logSink, proxyMetricPublisher, proxy.Options{
		AppName:         cfg.AppName,
		Metadata:        runtimeMetadata,
		HealthPaths:     healthPathSet(cfg.HealthURLs),
		MaxCaptureBytes: cfg.CaptureBodyLimit,
		Reporter:        reporter.Printf,
	})
	runContext, stopRun := context.WithCancel(context.Background())
	defer stopRun()

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

	go healthRunner.Run(runContext)

	serverErrors := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				serverErrors <- fmt.Errorf("server panic recovered: %v", recovered)
			}
		}()
		serverErrors <- server.ListenAndServe()
	}()

	var shutdownOnce sync.Once
	shutdownStarted := false
	shutdownDone := make(chan error, 1)
	initiateShutdown := func() {
		shutdownOnce.Do(func() {
			shutdownStarted = true
			stopRun()
			go func() {
				shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				shutdownDone <- server.Shutdown(shutdownContext)
			}()
		})
	}

	var runErr error
	if child == nil {
		select {
		case err = <-serverErrors:
			if !errors.Is(err, http.ErrServerClosed) {
				runErr = err
			}
		case sig := <-signals:
			reporter.Printf("received signal %v; shutting down", sig)
		}
		initiateShutdown()
	} else {
		reporter.Printf("inspector mode started child process %q (pid=%d)", child.Command(), child.PID())

		for {
			select {
			case err = <-serverErrors:
				if errors.Is(err, http.ErrServerClosed) {
					continue
				}
				if runErr == nil {
					runErr = err
				}
				reporter.Printf("proxy server stopped unexpectedly: %v", err)
				if forwardErr := child.ForwardSignal(syscall.SIGTERM); forwardErr != nil {
					reporter.Printf("failed to forward termination signal to inspector child: %v", forwardErr)
				}
				initiateShutdown()
			case sig := <-signals:
				reporter.Printf("forwarding signal %v to inspector child", sig)
				if forwardErr := child.ForwardSignal(sig); forwardErr != nil {
					reporter.Printf("failed to forward signal to inspector child: %v", forwardErr)
				}
				initiateShutdown()
			case <-child.Done():
				childStatus, _ := child.Result()
				initiateShutdown()
				if shutdownStarted {
					if shutdownErr := <-shutdownDone; shutdownErr != nil && !errors.Is(shutdownErr, http.ErrServerClosed) && !errors.Is(shutdownErr, context.Canceled) {
						reporter.Printf("server shutdown returned error: %v", shutdownErr)
					}
				}
				closeErr := closeAllWithTimeout(10*time.Second, closers...)
				if closeErr != nil {
					reporter.Printf("failed to flush and close sinks: %v", closeErr)
				}
				return childExitError(childStatus)
			}
		}
	}

	var shutdownErr error
	if shutdownStarted {
		shutdownErr = <-shutdownDone
	}
	closeErr := closeAllWithTimeout(10*time.Second, closers...)

	if shutdownErr != nil && !errors.Is(shutdownErr, context.Canceled) {
		return shutdownErr
	}
	if runErr != nil {
		return runErr
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

func closeAllWithTimeout(timeout time.Duration, closers ...contextCloser) error {
	closeContext, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return closeAll(closeContext, closers...)
}

func sanitizeStreamName(appName string, now time.Time, pid int) string {
	name := nonAlphaNumeric.ReplaceAllString(appName, "-")
	if name == "" {
		name = "cwproxy"
	}
	return fmt.Sprintf("%s-%d-%d", name, pid, now.Unix())
}

func resolveDefaultAppName(snapshot *metadata.Snapshot, hostname func() (string, error)) (string, error) {
	if name := metadata.InferDefaultAppName(snapshot); name != "" {
		return name, nil
	}
	return hostname()
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

func healthPathSet(urls []*url.URL) map[string]struct{} {
	if len(urls) == 0 {
		return nil
	}

	paths := make(map[string]struct{}, len(urls))
	for _, endpoint := range urls {
		if endpoint == nil {
			continue
		}
		path := endpoint.Path
		if path == "" {
			path = "/"
		}
		paths[path] = struct{}{}
	}
	return paths
}

func waitForInspectorPort(ctx context.Context, child *inspector.Child, signals <-chan os.Signal, reporter func(string, ...any)) (int, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		if result, exited := child.Result(); exited {
			return 0, childExitBeforeListenError(result)
		}

		ports, _ := child.ListeningPorts(ctx)
		if len(ports) > 0 {
			return ports[0], nil
		}

		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-child.Done():
			result, _ := child.Result()
			return 0, childExitBeforeListenError(result)
		case sig := <-signals:
			if reporter != nil {
				reporter("forwarding signal %v to inspector child during startup", sig)
			}
			if err := child.ForwardSignal(sig); err != nil && reporter != nil {
				reporter("failed to forward signal to inspector child during startup: %v", err)
			}
		case <-ticker.C:
		}
	}
}

func withEnvOverride(lookupEnv func(string) (string, bool), key, value string) func(string) (string, bool) {
	return func(current string) (string, bool) {
		if current == key {
			return value, true
		}
		return lookupEnv(current)
	}
}

func cleanupInspectorStartupFailure(child *inspector.Child, reporter func(string, ...any), startupErr error) error {
	if startupErr == nil || child == nil {
		return startupErr
	}

	if err := stopInspectorChild(child, 5*time.Second, reporter); err != nil {
		return errors.Join(startupErr, fmt.Errorf("stop inspector child after startup failure: %w", err))
	}
	return startupErr
}

func stopInspectorChild(child *inspector.Child, timeout time.Duration, reporter func(string, ...any)) error {
	if child == nil {
		return nil
	}
	if _, exited := child.Result(); exited {
		return nil
	}
	if reporter != nil {
		reporter("stopping inspector child process %q after startup failure", child.Command())
	}
	if err := child.Kill(); err != nil {
		if _, exited := child.Result(); exited {
			return nil
		}
		return err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-child.Done():
		return nil
	case <-timer.C:
		return errors.New("timed out waiting for inspector child to exit")
	}
}

type exitCodeError struct {
	code int
}

func (e *exitCodeError) Error() string {
	return fmt.Sprintf("exit with code %d", e.code)
}

func childExitError(status inspector.ExitStatus) error {
	if status.Code == 0 {
		return nil
	}
	if status.Code < 0 {
		return &exitCodeError{code: 1}
	}
	return &exitCodeError{code: status.Code}
}

func childExitBeforeListenError(status inspector.ExitStatus) error {
	if err := childExitError(status); err != nil {
		return err
	}
	return errors.New("inspector child exited before listen port detection")
}
