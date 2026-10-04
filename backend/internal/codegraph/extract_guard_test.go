package codegraph

import (
	"os"
	"strings"
	"testing"
)

// Frente 3 / 3B: extractRedacted is the only caller of Extractor.Extract, so
// every extractor -- a future Java one included -- has its docs, signatures
// and summaries redacted without having to remember to.
func TestExtractIsOnlyCalledThroughRedaction(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "extract.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "extractor.Extract(") || strings.Contains(string(src), ".Extract(rel") {
			t.Errorf("%s calls Extract directly; use extractRedacted", name)
		}
	}
}
