package cwlogs

import (
	"strings"
	"testing"

	"github.com/awsutils/cwproxy/internal/metrics"
)

func TestPartitionDatumsRejectsInvalidMetricDefinitions(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		data []metrics.Datum
	}{
		{
			name: "empty metric name",
			data: []metrics.Datum{{Name: "", Value: 1}},
		},
		{
			name: "reserved metric name",
			data: []metrics.Datum{{Name: "_aws", Value: 1}},
		},
		{
			name: "reserved dimension key",
			data: []metrics.Datum{{
				Name:  "Latency",
				Value: 1,
				Dimensions: map[string]string{
					"_q": "blocked",
				},
			}},
		},
		{
			name: "too many dimensions",
			data: []metrics.Datum{{
				Name:       "Latency",
				Value:      1,
				Dimensions: overLimitDimensions(),
			}},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := partitionDatums(testCase.data, reservedMetricRootKeys)
			if err == nil {
				t.Fatal("expected partitionDatums to fail")
			}
			if !strings.Contains(err.Error(), "cannot be encoded as EMF") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestAppendEMFRejectsNonJSONObjectPayload(t *testing.T) {
	t.Parallel()

	_, err := appendEMF([]byte("[]"), nil, emfEnvelope{})
	if err == nil {
		t.Fatal("expected appendEMF to fail")
	}
	if !strings.Contains(err.Error(), "not a JSON object") {
		t.Fatalf("error = %v", err)
	}
}

func overLimitDimensions() map[string]string {
	dimensions := make(map[string]string, 31)
	for index := 0; index < 31; index++ {
		dimensions[string(rune('a'+index))] = "x"
	}
	return dimensions
}
