package logging

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"sort"
	"strings"
)

func entryStructureHash(request Request, response Response) string {
	return hashStructureSignature(
		"queries:" + structureSignature(request.Queries) +
			"|request_body:" + structureSignature(request.Body) +
			"|response_body:" + structureSignature(response.Body),
	)
}

func queryStructureHash(values map[string]any) string {
	if len(values) == 0 {
		return ""
	}
	return hashStructureSignature(structureSignature(values))
}

func bodyStructureHash(value any) string {
	if value == nil {
		return ""
	}
	return hashStructureSignature(structureSignature(value))
}

func hashStructureSignature(signature string) string {
	sum := sha256.Sum256([]byte(signature))
	return hex.EncodeToString(sum[:16])
}

func structureSignature(value any) string {
	switch typed := value.(type) {
	case nil:
		return "n"
	case map[string]any:
		return objectSignature(typed)
	case []any:
		return arraySignature(typed)
	case []string:
		return arraySignature(stringsToAny(typed))
	default:
		return reflectSignature(reflect.ValueOf(value))
	}
}

func reflectSignature(value reflect.Value) string {
	if !value.IsValid() {
		return "n"
	}

	switch value.Kind() {
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return "v"
		}
		items := make(map[string]any, value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			items[iterator.Key().String()] = iterator.Value().Interface()
		}
		return objectSignature(items)
	case reflect.Slice, reflect.Array:
		items := make([]any, 0, value.Len())
		for index := 0; index < value.Len(); index++ {
			items = append(items, value.Index(index).Interface())
		}
		return arraySignature(items)
	default:
		return "v"
	}
}

func objectSignature(values map[string]any) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var builder strings.Builder
	builder.Grow(len(keys) * 8)
	builder.WriteString("o{")
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteByte(':')
		builder.WriteString(structureSignature(values[key]))
		builder.WriteByte(';')
	}
	builder.WriteByte('}')
	return builder.String()
}

func arraySignature(values []any) string {
	signatures := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		signature := structureSignature(value)
		if _, found := seen[signature]; found {
			continue
		}
		seen[signature] = struct{}{}
		signatures = append(signatures, signature)
	}
	sort.Strings(signatures)

	var builder strings.Builder
	builder.Grow(len(signatures) * 4)
	builder.WriteString("a[")
	for _, signature := range signatures {
		builder.WriteString(signature)
		builder.WriteByte(',')
	}
	builder.WriteByte(']')
	return builder.String()
}

func stringsToAny(values []string) []any {
	items := make([]any, 0, len(values))
	for _, value := range values {
		items = append(items, value)
	}
	return items
}
