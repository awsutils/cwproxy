package logging

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/awsutils/cwproxy/internal/metadata"
)

func TestMarshalProducesStableJSON(t *testing.T) {
	t.Parallel()

	entry := NewEntry(
		"cwproxy",
		Request{
			Time:    1700000000000,
			Host:    "example.com",
			Port:    8080,
			Path:    "/api/foo",
			Method:  http.MethodPost,
			URL:     "example.com:8080/api/foo?key=value",
			Queries: map[string]any{"key": "value"},
			Cookies: map[string]any{"session": "abc"},
			Headers: map[string]any{"Content-Type": "application/json"},
			Body:    map[string]any{"field": "value"},
		},
		Response{
			Time:       1700000000123,
			Status:     http.StatusOK,
			Headers:    map[string]any{"Content-Type": "application/json"},
			SetCookies: map[string]any{"session": "xyz"},
			Body:       map[string]any{"result": "ok"},
		},
		123*time.Millisecond,
	)

	got, err := Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	want := `{"_q":"cwproxy POST /api/foo 200 123.000ms","app_name":"cwproxy","delay":123.000,"request":{"time":1700000000000,"host":"example.com","port":8080,"path":"/api/foo","method":"POST","url":"example.com:8080/api/foo?key=value","queries":{"key":"value"},"cookies":{"session":"abc"},"headers":{"Content-Type":"application/json"},"body":{"field":"value"}},"response":{"time":1700000000123,"status":200,"headers":{"Content-Type":"application/json"},"set_cookies":{"session":"xyz"},"body":{"result":"ok"}}}`
	if string(got) != want {
		t.Fatalf("Marshal() = %s, want %s", got, want)
	}
}

func TestParseBody(t *testing.T) {
	t.Parallel()

	form := ParseBody("application/x-www-form-urlencoded", []byte("alpha=1&beta=2"), false)
	formMap, ok := form.(map[string]any)
	if !ok {
		t.Fatalf("form body = %T, want map[string]any", form)
	}
	if formMap["alpha"] != "1" || formMap["beta"] != "2" {
		t.Fatalf("form body = %#v", formMap)
	}

	jsonBody := ParseBody("application/json", []byte(`{"ok":true}`), false)
	jsonMap, ok := jsonBody.(map[string]any)
	if !ok || jsonMap["ok"] != true {
		t.Fatalf("json body = %#v", jsonBody)
	}

	truncated := ParseBody("application/json", []byte(`{"partial":`), true)
	if truncated != `{"partial":...(truncated)` {
		t.Fatalf("truncated body = %#v", truncated)
	}
}

func TestNormalizeHelpers(t *testing.T) {
	t.Parallel()

	values := NormalizeValues(url.Values{"x": {"1", "2"}, "a": {"b"}})
	if got, ok := values["a"].(string); !ok || got != "b" {
		t.Fatalf("NormalizeValues single = %#v", values["a"])
	}

	headers := NormalizeHeaders(http.Header{
		"Set-Cookie":   {"a=1"},
		"Content-Type": {"application/json"},
	}, "set-cookie")
	if _, found := headers["Set-Cookie"]; found {
		t.Fatalf("NormalizeHeaders unexpectedly retained Set-Cookie: %#v", headers)
	}

	cookies := NormalizeCookies([]*http.Cookie{{Name: "session", Value: "abc"}})
	if cookies["session"] != "abc" {
		t.Fatalf("NormalizeCookies = %#v", cookies)
	}
}

func TestNewEntryUsesUnknownMethodFallback(t *testing.T) {
	t.Parallel()

	entry := NewEntry(
		"cwproxy",
		Request{Path: "/health"},
		Response{Status: http.StatusOK},
		time.Millisecond,
	)

	if entry.Summary != "cwproxy UNKNOWN /health 200 1.000ms" {
		t.Fatalf("Summary = %q", entry.Summary)
	}
}

func TestDurationFormatsWithThreeDecimals(t *testing.T) {
	t.Parallel()

	if got := formatDelay(123456 * time.Microsecond).String(); got != "123.456" {
		t.Fatalf("String() = %q", got)
	}

	body, err := json.Marshal(struct {
		Delay Duration `json:"delay"`
	}{
		Delay: formatDelay(1500 * time.Microsecond),
	})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if string(body) != `{"delay":1.500}` {
		t.Fatalf("Marshal() = %s", body)
	}
}

func TestMarshalForCloudWatchInsertsNewlineAfterSummary(t *testing.T) {
	t.Parallel()

	entry := NewEntry(
		"cwproxy",
		Request{
			Method: http.MethodGet,
			Path:   "/health",
		},
		Response{
			Status: http.StatusOK,
		},
		1500*time.Microsecond,
	)

	body, err := MarshalForCloudWatch(entry)
	if err != nil {
		t.Fatalf("MarshalForCloudWatch returned error: %v", err)
	}

	want := "{\"_q\":\"cwproxy GET /health 200 1.500ms\",\n\"app_name\":\"cwproxy\",\"delay\":1.500,\"request\":{\"time\":0,\"host\":\"\",\"port\":0,\"path\":\"/health\",\"method\":\"GET\",\"url\":\"\",\"queries\":{},\"cookies\":{},\"headers\":{},\"body\":null},\"response\":{\"time\":0,\"status\":200,\"headers\":{},\"set_cookies\":{},\"body\":null}}"
	if string(body) != want {
		t.Fatalf("MarshalForCloudWatch() = %s, want %s", body, want)
	}
}

func TestMarshalIncludesAWSMetadata(t *testing.T) {
	t.Parallel()

	entry := NewEntry(
		"cwproxy",
		Request{
			Method: http.MethodGet,
			Path:   "/health",
		},
		Response{
			Status: http.StatusOK,
		},
		time.Millisecond,
	)
	entry.Metadata = &metadata.Snapshot{
		EC2: &metadata.EC2{
			InstanceID: "i-123",
			Region:     "ap-northeast-2",
		},
		EKS: &metadata.EKS{
			ClusterName: "demo-eks",
			PodName:     "cwproxy-123",
		},
	}

	body, err := Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	want := `{"_q":"cwproxy GET /health 200 1.000ms","app_name":"cwproxy","aws_meta":{"ec2":{"instance_id":"i-123","region":"ap-northeast-2"},"eks":{"cluster_name":"demo-eks","pod_name":"cwproxy-123"}},"delay":1.000,"request":{"time":0,"host":"","port":0,"path":"/health","method":"GET","url":"","queries":{},"cookies":{},"headers":{},"body":null},"response":{"time":0,"status":200,"headers":{},"set_cookies":{},"body":null}}`
	if string(body) != want {
		t.Fatalf("Marshal() = %s, want %s", body, want)
	}
}
