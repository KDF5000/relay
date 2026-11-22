package antigravity

import "testing"

func TestACPUsageIncludesCacheWithoutDoubleCounting(t *testing.T) {
	for _, test := range []struct {
		name                                        string
		input, read, write, output, total, expected float64
	}{
		{"separate cache", 2069, 7906, 0, 75, 10050, 9975},
		{"inclusive cache", 100, 5, 0, 20, 120, 100},
		{"cache write", 100, 5, 7, 20, 132, 112},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage := acpUsage(map[string]any{"usage": map[string]any{"inputTokens": test.input, "cachedReadTokens": test.read, "cachedWriteTokens": test.write, "outputTokens": test.output, "totalTokens": test.total, "thoughtTokens": 10}}, "model")
			if usage == nil || !usage.Valid() || *usage.InputTokens != int64(test.expected) || *usage.TotalTokens != int64(test.total) {
				t.Fatalf("usage=%+v", usage)
			}
		})
	}
	invalid := acpUsage(map[string]any{"usage": map[string]any{"inputTokens": 10, "outputTokens": 20, "totalTokens": 1000}}, "")
	if invalid != nil {
		t.Fatal("invented totals for invalid usage")
	}
}
