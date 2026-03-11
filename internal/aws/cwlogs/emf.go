package cwlogs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/awsutils/cwproxy/internal/logging"
	"github.com/awsutils/cwproxy/internal/metrics"
)

var (
	reservedLogRootKeys = map[string]struct{}{
		"_aws":     {},
		"_q":       {},
		"_t":       {},
		"app_name": {},
		"aws_meta": {},
		"delay":    {},
		"request":  {},
		"response": {},
	}
	reservedMetricRootKeys = map[string]struct{}{
		"_aws":     {},
		"_q":       {},
		"_t":       {},
		"app_name": {},
		"aws_meta": {},
	}
)

type emfEnvelope struct {
	Timestamp         int64          `json:"Timestamp"`
	CloudWatchMetrics []emfDirective `json:"CloudWatchMetrics"`
}

type emfDirective struct {
	Namespace  string          `json:"Namespace"`
	Dimensions [][]string      `json:"Dimensions"`
	Metrics    []emfMetricSpec `json:"Metrics"`
}

type emfMetricSpec struct {
	Name string `json:"Name"`
	Unit string `json:"Unit,omitempty"`
}

type emfField struct {
	name  string
	value any
}

type emfBatchState struct {
	datums          []metrics.Datum
	dimensionValues map[string]string
	metricValues    map[string]emfMetricSpecValue
	reservedRoot    map[string]struct{}
}

type emfMetricSpecValue struct {
	value float64
	unit  string
}

func marshalLogWithMetrics(entry logging.Entry, namespace string, data []metrics.Datum) ([]byte, error) {
	base, err := logging.MarshalForCloudWatch(entry)
	if err != nil {
		return nil, err
	}

	fields, envelope, err := buildEMF(namespace, entryTimestamp(entry), data, reservedLogRootKeys)
	if err != nil {
		return nil, err
	}

	return appendEMF(base, fields, envelope)
}

func marshalMetricEvent(appName, category, namespace string, timestamp int64, data []metrics.Datum, newlineAfterSummary bool) ([]byte, error) {
	fields, envelope, err := buildEMF(namespace, timestamp, data, reservedMetricRootKeys)
	if err != nil {
		return nil, err
	}
	category = logging.NormalizeCategory(category)
	includeIdentity := category != logging.CategoryHealth

	buffer := bytes.NewBuffer(make([]byte, 0, 256))
	buffer.WriteByte('{')

	if includeIdentity {
		if err := writeJSONField(buffer, "_q", buildMetricSummary(appName, data)); err != nil {
			return nil, err
		}
		if newlineAfterSummary {
			buffer.WriteString(",\n")
		} else {
			buffer.WriteByte(',')
		}
		if err := writeJSONField(buffer, "_t", category); err != nil {
			return nil, err
		}
		buffer.WriteByte(',')
		if err := writeJSONField(buffer, "app_name", appName); err != nil {
			return nil, err
		}
	} else {
		if err := writeJSONField(buffer, "_t", category); err != nil {
			return nil, err
		}
	}
	for _, field := range fields {
		buffer.WriteByte(',')
		if err := writeJSONField(buffer, field.name, field.value); err != nil {
			return nil, err
		}
	}
	buffer.WriteByte(',')
	if err := writeJSONField(buffer, "_aws", envelope); err != nil {
		return nil, err
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}

func buildEMF(namespace string, timestamp int64, data []metrics.Datum, reserved map[string]struct{}) ([]emfField, emfEnvelope, error) {
	batches, err := partitionDatums(data, reserved)
	if err != nil {
		return nil, emfEnvelope{}, err
	}
	if len(batches) != 1 {
		return nil, emfEnvelope{}, fmt.Errorf("expected one EMF batch, got %d", len(batches))
	}

	rootFields, directives, err := buildEMFParts(namespace, batches[0])
	if err != nil {
		return nil, emfEnvelope{}, err
	}

	return rootFields, emfEnvelope{
		Timestamp:         timestamp,
		CloudWatchMetrics: directives,
	}, nil
}

func partitionDatums(data []metrics.Datum, reserved map[string]struct{}) ([][]metrics.Datum, error) {
	if len(data) == 0 {
		return nil, nil
	}

	state := newBatchState(reserved)
	batches := make([][]metrics.Datum, 0, 1)

	for _, datum := range data {
		if !state.canAdd(datum) {
			if len(state.datums) == 0 {
				return nil, fmt.Errorf("metric %q cannot be encoded as EMF", datum.Name)
			}
			batches = append(batches, append([]metrics.Datum(nil), state.datums...))
			state = newBatchState(reserved)
			if !state.canAdd(datum) {
				return nil, fmt.Errorf("metric %q cannot be encoded as EMF", datum.Name)
			}
		}
		state.add(datum)
	}

	if len(state.datums) > 0 {
		batches = append(batches, append([]metrics.Datum(nil), state.datums...))
	}
	return batches, nil
}

func newBatchState(reserved map[string]struct{}) *emfBatchState {
	return &emfBatchState{
		dimensionValues: make(map[string]string),
		metricValues:    make(map[string]emfMetricSpecValue),
		reservedRoot:    reserved,
	}
}

func (s *emfBatchState) canAdd(datum metrics.Datum) bool {
	if datum.Name == "" {
		return false
	}
	if len(s.metricValues) >= 100 {
		return false
	}
	if _, found := s.reservedRoot[datum.Name]; found {
		return false
	}
	if _, found := s.dimensionValues[datum.Name]; found {
		return false
	}
	if existing, found := s.metricValues[datum.Name]; found {
		if existing.value != datum.Value || existing.unit != datum.Unit {
			return false
		}
	}
	if len(datum.Dimensions) > 30 {
		return false
	}

	for key, value := range datum.Dimensions {
		if key == "" {
			return false
		}
		if _, found := s.reservedRoot[key]; found {
			return false
		}
		if _, found := s.metricValues[key]; found {
			return false
		}
		if existing, found := s.dimensionValues[key]; found && existing != value {
			return false
		}
	}

	return true
}

func (s *emfBatchState) add(datum metrics.Datum) {
	s.datums = append(s.datums, datum)
	s.metricValues[datum.Name] = emfMetricSpecValue{
		value: datum.Value,
		unit:  datum.Unit,
	}
	for key, value := range datum.Dimensions {
		s.dimensionValues[key] = value
	}
}

func buildEMFParts(namespace string, data []metrics.Datum) ([]emfField, []emfDirective, error) {
	dimensionFields := make(map[string]string)
	metricFields := make(map[string]emfMetricSpecValue, len(data))
	grouped := make(map[string]*emfDirective)
	groupedMetricNames := make(map[string]map[string]struct{})

	for _, datum := range data {
		if existing, found := metricFields[datum.Name]; found {
			if existing.value != datum.Value || existing.unit != datum.Unit {
				return nil, nil, fmt.Errorf("metric %q has conflicting values in one EMF event", datum.Name)
			}
		} else {
			metricFields[datum.Name] = emfMetricSpecValue{
				value: datum.Value,
				unit:  datum.Unit,
			}
		}
		for key, value := range datum.Dimensions {
			dimensionFields[key] = value
		}

		keys := sortedKeys(datum.Dimensions)
		groupKey := strings.Join(keys, "\x00")
		directive, found := grouped[groupKey]
		if !found {
			directive = &emfDirective{
				Namespace:  namespace,
				Dimensions: [][]string{keys},
			}
			grouped[groupKey] = directive
			groupedMetricNames[groupKey] = make(map[string]struct{})
		}
		if _, found := groupedMetricNames[groupKey][datum.Name]; !found {
			directive.Metrics = append(directive.Metrics, emfMetricSpec{
				Name: datum.Name,
				Unit: datum.Unit,
			})
			groupedMetricNames[groupKey][datum.Name] = struct{}{}
		}
	}

	fields := make([]emfField, 0, len(dimensionFields)+len(metricFields))
	for _, key := range sortedMapKeys(dimensionFields) {
		fields = append(fields, emfField{name: key, value: dimensionFields[key]})
	}
	for _, key := range sortedMapKeys(metricFields) {
		fields = append(fields, emfField{name: key, value: metricFields[key].value})
	}

	groupKeys := make([]string, 0, len(grouped))
	for key := range grouped {
		groupKeys = append(groupKeys, key)
	}
	sort.Strings(groupKeys)

	directives := make([]emfDirective, 0, len(groupKeys))
	for _, key := range groupKeys {
		directive := grouped[key]
		sort.Slice(directive.Metrics, func(i, j int) bool {
			return directive.Metrics[i].Name < directive.Metrics[j].Name
		})
		directives = append(directives, *directive)
	}

	return fields, directives, nil
}

func appendEMF(base []byte, fields []emfField, envelope emfEnvelope) ([]byte, error) {
	if len(base) == 0 || base[len(base)-1] != '}' {
		return nil, fmt.Errorf("log entry is not a JSON object")
	}

	buffer := bytes.NewBuffer(make([]byte, 0, len(base)+256))
	buffer.Write(base[:len(base)-1])

	for _, field := range fields {
		buffer.WriteByte(',')
		if err := writeJSONField(buffer, field.name, field.value); err != nil {
			return nil, err
		}
	}

	buffer.WriteByte(',')
	if err := writeJSONField(buffer, "_aws", envelope); err != nil {
		return nil, err
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}

func writeJSONField(buffer *bytes.Buffer, key string, value any) error {
	name, err := json.Marshal(key)
	if err != nil {
		return err
	}
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}

	buffer.Write(name)
	buffer.WriteByte(':')
	buffer.Write(body)
	return nil
}

func buildMetricSummary(appName string, data []metrics.Datum) string {
	names := make([]string, 0, len(data))
	seen := make(map[string]struct{}, len(data))
	for _, datum := range data {
		if _, found := seen[datum.Name]; found {
			continue
		}
		seen[datum.Name] = struct{}{}
		names = append(names, datum.Name)
	}
	sort.Strings(names)

	summary := "METRIC"
	if len(names) > 0 {
		summary += " " + strings.Join(names, ",")
	}
	if strings.TrimSpace(appName) == "" {
		return summary
	}
	return appName + " " + summary
}

func entryTimestamp(entry logging.Entry) int64 {
	switch {
	case entry.Response.Time > 0:
		return entry.Response.Time
	case entry.Request.Time > 0:
		return entry.Request.Time
	default:
		return time.Now().UnixMilli()
	}
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
