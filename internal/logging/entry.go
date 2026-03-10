package logging

import (
	"bytes"
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Entry struct {
	Summary  string   `json:"_q"`
	AppName  string   `json:"app_name"`
	Delay    int64    `json:"delay"`
	Request  Request  `json:"request"`
	Response Response `json:"response"`
}

type Request struct {
	Time    int64          `json:"time"`
	Host    string         `json:"host"`
	Port    int            `json:"port"`
	Path    string         `json:"path"`
	Method  string         `json:"method"`
	URL     string         `json:"url"`
	Queries map[string]any `json:"queries"`
	Cookies map[string]any `json:"cookies"`
	Headers map[string]any `json:"headers"`
	Body    any            `json:"body"`
}

type Response struct {
	Time       int64          `json:"time"`
	Status     int            `json:"status"`
	Headers    map[string]any `json:"headers"`
	SetCookies map[string]any `json:"set_cookies"`
	Body       any            `json:"body"`
}

func NewEntry(appName string, request Request, response Response, delay time.Duration) Entry {
	return Entry{
		Summary:  buildSummary(appName, request.Method, request.Path, response.Status, delay),
		AppName:  appName,
		Delay:    delay.Milliseconds(),
		Request:  ensureRequest(request),
		Response: ensureResponse(response),
	}
}

func Marshal(entry Entry) ([]byte, error) {
	return json.Marshal(ensureEntry(entry))
}

func ParseBody(contentType string, body []byte, truncated bool) any {
	if len(body) == 0 {
		return nil
	}
	if truncated {
		return string(body) + "...(truncated)"
	}

	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return string(body)
	}

	switch strings.ToLower(mediaType) {
	case "application/json":
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		var payload any
		if err := decoder.Decode(&payload); err == nil {
			return payload
		}
	case "application/x-www-form-urlencoded":
		values, err := url.ParseQuery(string(body))
		if err == nil {
			return NormalizeValues(values)
		}
	}

	return string(body)
}

func NormalizeHeaders(headers http.Header, excluded ...string) map[string]any {
	blocked := make(map[string]struct{}, len(excluded))
	for _, key := range excluded {
		blocked[strings.ToLower(key)] = struct{}{}
	}

	values := make(url.Values, len(headers))
	for key, items := range headers {
		if _, skip := blocked[strings.ToLower(key)]; skip {
			continue
		}
		copied := append([]string(nil), items...)
		sort.Strings(copied)
		values[key] = copied
	}
	return NormalizeValues(values)
}

func NormalizeValues(values url.Values) map[string]any {
	normalized := make(map[string]any, len(values))
	for key, items := range values {
		switch len(items) {
		case 0:
			normalized[key] = ""
		case 1:
			normalized[key] = items[0]
		default:
			copied := append([]string(nil), items...)
			sort.Strings(copied)
			normalized[key] = copied
		}
	}
	return normalized
}

func NormalizeCookies(cookies []*http.Cookie) map[string]any {
	grouped := make(url.Values, len(cookies))
	for _, cookie := range cookies {
		if cookie == nil {
			continue
		}
		grouped.Add(cookie.Name, cookie.Value)
	}
	return NormalizeValues(grouped)
}

func buildSummary(appName, method, path string, status int, delay time.Duration) string {
	if method == "" {
		method = "UNKNOWN"
	}
	return strings.TrimSpace(appName + " " + method + " " + path + " " + strconv.Itoa(status) + " " + strconv.FormatInt(delay.Milliseconds(), 10) + "ms")
}

func ensureEntry(entry Entry) Entry {
	entry.Request = ensureRequest(entry.Request)
	entry.Response = ensureResponse(entry.Response)
	return entry
}

func ensureRequest(request Request) Request {
	if request.Queries == nil {
		request.Queries = map[string]any{}
	}
	if request.Cookies == nil {
		request.Cookies = map[string]any{}
	}
	if request.Headers == nil {
		request.Headers = map[string]any{}
	}
	return request
}

func ensureResponse(response Response) Response {
	if response.Headers == nil {
		response.Headers = map[string]any{}
	}
	if response.SetCookies == nil {
		response.SetCookies = map[string]any{}
	}
	return response
}
