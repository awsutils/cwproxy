package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/awsutils/cwproxy/internal/inspector"
	"github.com/awsutils/cwproxy/internal/metadata"
)

func TestResolveAWSRegion(t *testing.T) {
	t.Run("uses AWS_REGION first", func(t *testing.T) {
		t.Setenv("AWS_REGION", "us-east-1")
		t.Setenv("AWS_DEFAULT_REGION", "us-west-2")

		region, source := resolveAWSRegion(&metadata.Snapshot{
			EC2: &metadata.EC2{Region: "ap-northeast-2"},
		})
		if region != "us-east-1" || source != "AWS_REGION" {
			t.Fatalf("resolveAWSRegion() = (%q, %q)", region, source)
		}
	})

	t.Run("uses AWS_DEFAULT_REGION when AWS_REGION is unset", func(t *testing.T) {
		t.Setenv("AWS_REGION", "")
		t.Setenv("AWS_DEFAULT_REGION", "us-west-2")

		region, source := resolveAWSRegion(&metadata.Snapshot{
			EC2: &metadata.EC2{Region: "ap-northeast-2"},
		})
		if region != "us-west-2" || source != "AWS_DEFAULT_REGION" {
			t.Fatalf("resolveAWSRegion() = (%q, %q)", region, source)
		}
	})

	t.Run("falls back to runtime metadata", func(t *testing.T) {
		t.Setenv("AWS_REGION", "")
		t.Setenv("AWS_DEFAULT_REGION", "")

		region, source := resolveAWSRegion(&metadata.Snapshot{
			ECS: &metadata.ECS{
				TaskARN: "arn:aws:ecs:eu-west-1:123456789012:task/abc",
			},
		})
		if region != "eu-west-1" || source != "runtime metadata" {
			t.Fatalf("resolveAWSRegion() = (%q, %q)", region, source)
		}
	})

	t.Run("returns empty when nothing is available", func(t *testing.T) {
		t.Setenv("AWS_REGION", "")
		t.Setenv("AWS_DEFAULT_REGION", "")

		region, source := resolveAWSRegion(nil)
		if region != "" || source != "" {
			t.Fatalf("resolveAWSRegion() = (%q, %q)", region, source)
		}
	})
}

func TestResolveDefaultAppName(t *testing.T) {
	t.Run("uses eks deployment name first", func(t *testing.T) {
		name, err := resolveDefaultAppName(&metadata.Snapshot{
			EKS: &metadata.EKS{DeploymentName: "orders-api"},
			ECS: &metadata.ECS{TaskFamily: "orders-task"},
		}, func() (string, error) {
			return "host-name", nil
		})
		if err != nil {
			t.Fatalf("resolveDefaultAppName() error = %v", err)
		}
		if name != "orders-api" {
			t.Fatalf("resolveDefaultAppName() = %q, want orders-api", name)
		}
	})

	t.Run("uses ecs task family before hostname", func(t *testing.T) {
		name, err := resolveDefaultAppName(&metadata.Snapshot{
			ECS: &metadata.ECS{TaskFamily: "orders-task"},
		}, func() (string, error) {
			return "host-name", nil
		})
		if err != nil {
			t.Fatalf("resolveDefaultAppName() error = %v", err)
		}
		if name != "orders-task" {
			t.Fatalf("resolveDefaultAppName() = %q, want orders-task", name)
		}
	})

	t.Run("falls back to hostname", func(t *testing.T) {
		name, err := resolveDefaultAppName(nil, func() (string, error) {
			return "host-name", nil
		})
		if err != nil {
			t.Fatalf("resolveDefaultAppName() error = %v", err)
		}
		if name != "host-name" {
			t.Fatalf("resolveDefaultAppName() = %q, want host-name", name)
		}
	})
}

func TestWaitForInspectorPortReturnsLowestPort(t *testing.T) {
	t.Parallel()

	manifestPath := filepath.Join(t.TempDir(), "ports.txt")
	child := startMainHelper(t, "listen", manifestPath, "200", "1500", "2")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	port, err := waitForInspectorPort(ctx, child, make(chan os.Signal), nil)
	if err != nil {
		t.Fatalf("waitForInspectorPort() error = %v", err)
	}

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}

	expected := lowestPort(t, strings.Split(strings.TrimSpace(string(manifest)), ","))
	if port != expected {
		t.Fatalf("waitForInspectorPort() = %d, want %d", port, expected)
	}

	<-child.Done()
}

func TestWaitForInspectorPortFailsWhenChildExitsBeforeListening(t *testing.T) {
	t.Parallel()

	child := startMainHelper(t, "exit", "0")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := waitForInspectorPort(ctx, child, make(chan os.Signal), nil)
	if err == nil {
		t.Fatal("waitForInspectorPort() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "before listen port detection") {
		t.Fatalf("waitForInspectorPort() error = %v, want startup exit message", err)
	}
}

func TestWithEnvOverride(t *testing.T) {
	t.Parallel()

	lookupEnv := withEnvOverride(func(key string) (string, bool) {
		if key == "OTHER" {
			return "value", true
		}
		return "", false
	}, "APP_PORT", "19090")

	if value, ok := lookupEnv("APP_PORT"); !ok || value != "19090" {
		t.Fatalf("lookupEnv(APP_PORT) = (%q, %t), want (19090, true)", value, ok)
	}
	if value, ok := lookupEnv("OTHER"); !ok || value != "value" {
		t.Fatalf("lookupEnv(OTHER) = (%q, %t), want (value, true)", value, ok)
	}
}

func TestMainHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_CWPROXY_MAIN_HELPER") != "1" {
		return
	}

	args := helperArgs()
	if len(args) == 0 {
		os.Exit(97)
	}

	switch args[0] {
	case "exit":
		if len(args) != 2 {
			os.Exit(96)
		}
		code, err := strconv.Atoi(args[1])
		if err != nil {
			os.Exit(95)
		}
		os.Exit(code)
	case "listen":
		if len(args) != 5 {
			os.Exit(94)
		}

		delayMS, err := strconv.Atoi(args[2])
		if err != nil {
			os.Exit(93)
		}
		holdMS, err := strconv.Atoi(args[3])
		if err != nil {
			os.Exit(92)
		}
		listenerCount, err := strconv.Atoi(args[4])
		if err != nil || listenerCount < 1 {
			os.Exit(91)
		}

		time.Sleep(time.Duration(delayMS) * time.Millisecond)

		listeners := make([]net.Listener, 0, listenerCount)
		ports := make([]string, 0, listenerCount)
		for index := 0; index < listenerCount; index++ {
			listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
			if listenErr != nil {
				os.Exit(90)
			}
			listeners = append(listeners, listener)
			address, ok := listener.Addr().(*net.TCPAddr)
			if !ok {
				os.Exit(89)
			}
			ports = append(ports, strconv.Itoa(address.Port))
		}

		if writeErr := os.WriteFile(args[1], []byte(strings.Join(ports, ",")), 0o600); writeErr != nil {
			os.Exit(88)
		}

		time.Sleep(time.Duration(holdMS) * time.Millisecond)
		for _, listener := range listeners {
			_ = listener.Close()
		}
		os.Exit(0)
	default:
		os.Exit(87)
	}
}

func startMainHelper(t *testing.T, helperArgs ...string) *inspector.Child {
	t.Helper()

	args := append([]string{"-test.run=TestMainHelperProcess", "--"}, helperArgs...)
	child, err := inspector.Start(
		os.Args[0],
		args,
		append(os.Environ(), "GO_WANT_CWPROXY_MAIN_HELPER=1"),
		nil,
		io.Discard,
		io.Discard,
	)
	if err != nil {
		t.Fatalf("inspector.Start() error = %v", err)
	}
	return child
}

func helperArgs() []string {
	args := os.Args
	for index, current := range args {
		if current == "--" {
			return args[index+1:]
		}
	}
	return nil
}

func lowestPort(t *testing.T, raw []string) int {
	t.Helper()

	lowest := 0
	for _, item := range raw {
		port, err := strconv.Atoi(strings.TrimSpace(item))
		if err != nil {
			t.Fatalf("Atoi returned error: %v", err)
		}
		if lowest == 0 || port < lowest {
			lowest = port
		}
	}
	return lowest
}
