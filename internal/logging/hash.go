package logging

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"sort"
	"strings"
)

const (
	shortHashBytes             = 3
	maxStructureSignatureDepth = 32
)

type signatureState struct {
	active map[signatureVisit]struct{}
}

type signatureVisit struct {
	kind reflect.Kind
	typ  reflect.Type
	ptr  uintptr
}

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
	return hex.EncodeToString(sum[:shortHashBytes])
}

func structureSignature(value any) string {
	return structureSignatureValue(reflect.ValueOf(value), 0, &signatureState{
		active: make(map[signatureVisit]struct{}),
	})
}

func structureSignatureValue(value reflect.Value, depth int, state *signatureState) string {
	if !value.IsValid() {
		return "n"
	}

	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return "n"
		}
		return structureSignatureValue(value.Elem(), depth, state)
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return "v"
		}
		if value.IsNil() {
			return "o{}"
		}
		if depth >= maxStructureSignatureDepth {
			return "x"
		}
		release, cyclic := state.enter(value)
		if cyclic {
			return "c"
		}
		defer release()
		return objectSignature(value, depth+1, state)
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return "a[]"
		}
		if depth >= maxStructureSignatureDepth {
			return "x"
		}
		if value.Kind() == reflect.Slice {
			release, cyclic := state.enter(value)
			if cyclic {
				return "c"
			}
			defer release()
		}
		return arraySignature(value, depth+1, state)
	default:
		return "v"
	}
}

func objectSignature(value reflect.Value, depth int, state *signatureState) string {
	keys := mapKeys(value)
	var builder strings.Builder
	builder.Grow(len(keys) * 8)
	builder.WriteString("o{")
	for _, key := range keys {
		keyValue := reflect.ValueOf(key)
		if keyValue.Type() != value.Type().Key() {
			keyValue = keyValue.Convert(value.Type().Key())
		}
		builder.WriteString(key)
		builder.WriteByte(':')
		builder.WriteString(structureSignatureValue(value.MapIndex(keyValue), depth, state))
		builder.WriteByte(';')
	}
	builder.WriteByte('}')
	return builder.String()
}

func arraySignature(value reflect.Value, depth int, state *signatureState) string {
	signatures := make([]string, 0, value.Len())
	seen := make(map[string]struct{}, value.Len())
	for index := 0; index < value.Len(); index++ {
		signature := structureSignatureValue(value.Index(index), depth, state)
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

func mapKeys(value reflect.Value) []string {
	keys := make([]string, 0, value.Len())
	iterator := value.MapRange()
	for iterator.Next() {
		keys = append(keys, iterator.Key().String())
	}
	sort.Strings(keys)
	return keys
}

func (s *signatureState) enter(value reflect.Value) (func(), bool) {
	if !value.IsValid() || value.IsNil() {
		return func() {}, false
	}

	visit := signatureVisit{
		kind: value.Kind(),
		typ:  value.Type(),
		ptr:  value.Pointer(),
	}
	if visit.ptr == 0 {
		return func() {}, false
	}
	if _, found := s.active[visit]; found {
		return nil, true
	}
	s.active[visit] = struct{}{}
	return func() {
		delete(s.active, visit)
	}, false
}
