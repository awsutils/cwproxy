package logging

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/awsutils/cwproxy/internal/metadata"
)

type Entry struct {
	Summary    string             `json:"_q,omitempty"`
	Type       string             `json:"_t"`
	AppName    string             `json:"app_name,omitempty"`
	Metadata   *metadata.Snapshot `json:"aws_meta,omitempty"`
	GlobalHash string             `json:"global_hash"`
	Delay      Duration           `json:"delay"`
	Request    Request            `json:"request"`
	Response   Response           `json:"response"`
}

const (
	CategoryTraffic  = "TRAFFIC"
	CategoryHealth   = "HEALTH"
	maxXMLParseDepth = 64
)

var errXMLDepthExceeded = errors.New("xml nesting depth exceeded")

type Duration float64

type Request struct {
	Time        int64          `json:"time"`
	Host        string         `json:"host"`
	Port        int            `json:"port"`
	Path        string         `json:"path"`
	Method      string         `json:"method"`
	URL         string         `json:"url"`
	Queries     map[string]any `json:"queries"`
	QueriesHash string         `json:"queries_hash,omitempty"`
	Cookies     map[string]any `json:"cookies"`
	Headers     map[string]any `json:"headers"`
	Body        any            `json:"body"`
	BodyRaw     string         `json:"body_raw,omitempty"`
	BodyHash    string         `json:"body_hash,omitempty"`
}

type Response struct {
	Time       int64          `json:"time"`
	Status     int            `json:"status"`
	Headers    map[string]any `json:"headers"`
	SetCookies map[string]any `json:"set_cookies"`
	Body       any            `json:"body"`
	BodyRaw    string         `json:"body_raw,omitempty"`
	BodyHash   string         `json:"body_hash,omitempty"`
}

func NewEntry(appName string, request Request, response Response, delay time.Duration) Entry {
	return newEntry(CategoryTraffic, appName, request, response, delay)
}

func NewHealthEntry(appName string, request Request, response Response, delay time.Duration) Entry {
	return newEntry(CategoryHealth, appName, request, response, delay)
}

func newEntry(category, appName string, request Request, response Response, delay time.Duration) Entry {
	delayValue := formatDelay(delay)
	return Entry{
		Type:     NormalizeCategory(category),
		Summary:  buildSummary(appName, request.Method, request.Path, response.Status, delayValue),
		AppName:  appName,
		Delay:    delayValue,
		Request:  ensureRequest(request),
		Response: ensureResponse(response),
	}
}

func Marshal(entry Entry) ([]byte, error) {
	return json.Marshal(ensureEntry(entry))
}

func MarshalForCloudWatch(entry Entry) ([]byte, error) {
	entry = ensureEntry(entry)
	body, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	if entry.Type == CategoryHealth || entry.Summary == "" {
		return body, nil
	}
	return insertNewlineAfterSummary(body), nil
}

func ParseBody(contentType string, body []byte, truncated bool) any {
	if len(body) == 0 {
		return nil
	}
	rawBody := FormatBodyRaw(body, truncated)
	if truncated {
		return rawBody
	}

	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return rawBody
	}

	mediaType = strings.ToLower(mediaType)
	switch {
	case mediaType == "application/json":
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		var payload any
		if err := decoder.Decode(&payload); err == nil {
			return payload
		}
	case mediaType == "application/x-www-form-urlencoded":
		values, err := url.ParseQuery(string(body))
		if err == nil {
			return NormalizeValues(values)
		}
	case isXMLMediaType(mediaType):
		payload, err := parseXMLBody(body)
		if err == nil {
			return payload
		}
	}

	return rawBody
}

func FormatBodyRaw(body []byte, truncated bool) string {
	if len(body) == 0 {
		return ""
	}
	raw := string(body)
	if truncated {
		return raw + "...(truncated)"
	}
	return raw
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

func buildSummary(appName, method, path string, status int, delay Duration) string {
	if method == "" {
		method = "UNKNOWN"
	}
	return strings.TrimSpace(appName + " " + method + " " + path + " " + strconv.Itoa(status) + " " + delay.String() + "ms")
}

func formatDelay(delay time.Duration) Duration {
	return Duration(float64(delay) / float64(time.Millisecond))
}

func (d Duration) String() string {
	return fmt.Sprintf("%.3f", float64(d))
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(d.String()), nil
}

func ensureEntry(entry Entry) Entry {
	entry.Request = ensureRequest(entry.Request)
	entry.Response = ensureResponse(entry.Response)
	entry.Type = NormalizeCategory(entry.Type)
	if entry.Type == CategoryHealth {
		entry.Summary = ""
		entry.AppName = ""
	}
	if entry.GlobalHash == "" {
		entry.GlobalHash = entryStructureHash(entry.Request, entry.Response)
	}
	return entry
}

func NormalizeCategory(category string) string {
	switch strings.ToUpper(strings.TrimSpace(category)) {
	case CategoryHealth:
		return CategoryHealth
	default:
		return CategoryTraffic
	}
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
	if request.QueriesHash == "" {
		request.QueriesHash = queryStructureHash(request.Queries)
	}
	if request.BodyHash == "" {
		request.BodyHash = bodyStructureHash(request.Body)
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
	if response.BodyHash == "" {
		response.BodyHash = bodyStructureHash(response.Body)
	}
	return response
}

func insertNewlineAfterSummary(body []byte) []byte {
	inString := false
	escaped := false

	for index, current := range body {
		if escaped {
			escaped = false
			continue
		}

		switch current {
		case '\\':
			if inString {
				escaped = true
			}
		case '"':
			inString = !inString
		case ',':
			if !inString {
				formatted := make([]byte, 0, len(body)+1)
				formatted = append(formatted, body[:index+1]...)
				formatted = append(formatted, '\n')
				formatted = append(formatted, body[index+1:]...)
				return formatted
			}
		}
	}

	return body
}

func isXMLMediaType(mediaType string) bool {
	return mediaType == "application/xml" || mediaType == "text/xml" || strings.HasSuffix(mediaType, "+xml")
}

func parseXMLBody(body []byte) (any, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))

	for {
		token, err := decoder.Token()
		if err != nil {
			if err == io.EOF {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}

		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}

		element, err := decodeXMLElement(decoder, start, 1)
		if err != nil {
			return nil, err
		}
		return map[string]any{xmlName(start.Name): element}, nil
	}
}

func decodeXMLElement(decoder *xml.Decoder, start xml.StartElement, depth int) (any, error) {
	if depth > maxXMLParseDepth {
		return nil, errXMLDepthExceeded
	}

	fields := make(map[string]any, len(start.Attr))
	for _, attribute := range start.Attr {
		fields["@"+xmlName(attribute.Name)] = attribute.Value
	}

	textParts := make([]string, 0, 1)

	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}

		switch value := token.(type) {
		case xml.StartElement:
			child, err := decodeXMLElement(decoder, value, depth+1)
			if err != nil {
				return nil, err
			}
			appendXMLChild(fields, xmlName(value.Name), child)
		case xml.CharData:
			text := strings.TrimSpace(string(value))
			if text != "" {
				textParts = append(textParts, text)
			}
		case xml.EndElement:
			if value.Name.Local != start.Name.Local || value.Name.Space != start.Name.Space {
				continue
			}

			text := strings.Join(textParts, " ")
			if len(fields) == 0 {
				if text == "" {
					return map[string]any{}, nil
				}
				return text, nil
			}
			if text != "" {
				fields["#text"] = text
			}
			return fields, nil
		}
	}
}

func appendXMLChild(fields map[string]any, key string, value any) {
	if existing, found := fields[key]; found {
		items, ok := existing.([]any)
		if !ok {
			fields[key] = []any{existing, value}
			return
		}
		fields[key] = append(items, value)
		return
	}
	fields[key] = value
}

func xmlName(name xml.Name) string {
	if name.Local != "" {
		return name.Local
	}
	return name.Space
}
