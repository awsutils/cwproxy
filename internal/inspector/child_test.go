package inspector

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
)

func TestStartCapturesExitCode(t *testing.T) {
	t.Parallel()

	child := startHelper(t, "exit", "7")
	<-child.Done()

	result, exited := child.Result()
	if !exited {
		t.Fatal("expected child to be exited")
	}
	if result.Code != 7 {
		t.Fatalf("exit code = %d, want 7", result.Code)
	}
}

func TestListeningPortsReturnsLowestPort(t *testing.T) {
	t.Parallel()

	manifestPath := filepath.Join(t.TempDir(), "ports.txt")
	child := startHelper(t, "listen", manifestPath, "300", "1200", "2")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	detectedPort := 0
	for detectedPort == 0 {
		ports, _ := child.ListeningPorts(ctx)
		if len(ports) > 0 {
			detectedPort = ports[0]
			break
		}

		select {
		case <-child.Done():
			result, _ := child.Result()
			t.Fatalf("child exited before listen port detection: %#v", result)
		case <-ctx.Done():
			t.Fatalf("timed out waiting for listen port: %v", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}

	expected := lowestPort(t, strings.Split(strings.TrimSpace(string(manifest)), ","))
	if detectedPort != expected {
		t.Fatalf("detected port = %d, want %d", detectedPort, expected)
	}

	<-child.Done()
	result, _ := child.Result()
	if result.Code != 0 {
		t.Fatalf("exit code = %d, want 0", result.Code)
	}
}

func TestInspectorHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_INSPECTOR_HELPER") != "1" {
		return
	}

	args := helperArgs()
	switch args[0] {
	case "exit":
		code, err := strconv.Atoi(args[1])
		if err != nil {
			os.Exit(99)
		}
		os.Exit(code)
	case "listen":
		if len(args) != 5 {
			os.Exit(98)
		}

		delayMS, err := strconv.Atoi(args[2])
		if err != nil {
			os.Exit(97)
		}
		holdMS, err := strconv.Atoi(args[3])
		if err != nil {
			os.Exit(96)
		}
		listenerCount, err := strconv.Atoi(args[4])
		if err != nil || listenerCount < 1 {
			os.Exit(95)
		}

		time.Sleep(time.Duration(delayMS) * time.Millisecond)

		listeners := make([]net.Listener, 0, listenerCount)
		ports := make([]string, 0, listenerCount)
		for index := 0; index < listenerCount; index++ {
			listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
			if listenErr != nil {
				os.Exit(94)
			}
			listeners = append(listeners, listener)
			address, ok := listener.Addr().(*net.TCPAddr)
			if !ok {
				os.Exit(93)
			}
			ports = append(ports, strconv.Itoa(address.Port))
		}

		if writeErr := os.WriteFile(args[1], []byte(strings.Join(ports, ",")), 0o600); writeErr != nil {
			os.Exit(92)
		}

		time.Sleep(time.Duration(holdMS) * time.Millisecond)
		for _, listener := range listeners {
			_ = listener.Close()
		}
		os.Exit(0)
	}

	os.Exit(91)
}

func startHelper(t *testing.T, helperArgs ...string) *Child {
	t.Helper()

	args := append([]string{"-test.run=TestInspectorHelperProcess", "--"}, helperArgs...)
	child, err := Start(os.Args[0], args, append(os.Environ(), "GO_WANT_INSPECTOR_HELPER=1"), nil, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
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
