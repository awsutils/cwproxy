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
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/awsutils/cwproxy/internal/aws/cwlogs"
	"github.com/awsutils/cwproxy/internal/aws/cwmetrics"
	"github.com/awsutils/cwproxy/internal/config"
	"github.com/awsutils/cwproxy/internal/health"
	"github.com/awsutils/cwproxy/internal/logging"
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

	stdoutSink := logging.NewStdoutSink(os.Stdout)
	logSink := logging.Sink(stdoutSink)
	metricPublisher := metrics.Publisher(metrics.NopPublisher{})

	closers := []contextCloser{stdoutSink}

	awsRegion := os.Getenv("AWS_REGION")
	if awsRegion == "" {
		awsRegion = os.Getenv("AWS_DEFAULT_REGION")
	}

	if awsRegion == "" {
		reporter.Printf("AWS region is not configured; CloudWatch Logs and Metrics are disabled")
	} else {
		awsConfig, awsErr := awscfg.LoadDefaultConfig(rootContext)
		if awsErr != nil {
			reporter.Printf("failed to load AWS configuration: %v", awsErr)
		} else {
			logStreamName := sanitizeStreamName(cfg.AppName, time.Now(), os.Getpid())

			cwLogSink, sinkErr := cwlogs.New(rootContext, cloudwatchlogs.NewFromConfig(awsConfig), cfg.LogGroupName, cwlogs.Options{
				StreamName: logStreamName,
				Reporter:   reporter.Printf,
			})
			if sinkErr != nil {
				reporter.Printf("failed to initialize CloudWatch Logs sink: %v", sinkErr)
			} else {
				logSink = logging.NewMultiSink(stdoutSink, cwLogSink)
				closers = append(closers, cwLogSink)
			}

			cloudWatchPublisher := metrics.NewAsyncPublisher(
				cwmetrics.New(cloudwatch.NewFromConfig(awsConfig), "sniff2cw/"+cfg.AppName),
				metrics.Options{
					Reporter: reporter.Printf,
				},
			)
			metricPublisher = cloudWatchPublisher
			closers = append(closers, cloudWatchPublisher)
		}
	}

	handler := proxy.New(cfg.TargetURL, logSink, metricPublisher, proxy.Options{
		AppName:         cfg.AppName,
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

	healthRunner := health.NewRunner(cfg.HealthURLs, metricPublisher, health.Options{
		Interval: cfg.HealthInterval,
		Reporter: reporter.Printf,
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
