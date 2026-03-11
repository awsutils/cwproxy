package logging

import (
	"strings"
	"testing"
)

func TestBodyStructureHashHandlesCyclicMaps(t *testing.T) {
	t.Parallel()

	body := map[string]any{}
	body["self"] = body

	hash := bodyStructureHash(body)
	if hash == "" {
		t.Fatal("expected cyclic body hash to be populated")
	}
	if len(hash) != 6 {
		t.Fatalf("expected 6-character body hash, got %q", hash)
	}
}

func TestStructureSignatureCapsDeepNesting(t *testing.T) {
	t.Parallel()

	value := map[string]any{}
	current := value
	for index := 0; index < maxStructureSignatureDepth+4; index++ {
		next := map[string]any{}
		current["child"] = next
		current = next
	}

	signature := structureSignature(value)
	if !strings.Contains(signature, "x") {
		t.Fatalf("expected depth marker in signature, got %q", signature)
	}
}
