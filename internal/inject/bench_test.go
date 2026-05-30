package inject

import (
	"strconv"
	"testing"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
)

// These benchmarks quantify the CPU the proxy adds per request on the injection
// hot path (everything except the MuninnDB recall RTT): selection, budget
// packing, and format-specific injection. The design premise is that this is
// negligible next to the network call.

func benchMemories(n int) []memory {
	m := make([]memory, n)
	for i := range m {
		m[i] = memory{
			ID:      "id" + strconv.Itoa(i),
			Concept: "concept number " + strconv.Itoa(i),
			Content: "Some recalled memory content about topic " + strconv.Itoa(i) + " with a sentence of detail.",
			Score:   0.9 - float64(i)*0.03,
		}
	}
	return m
}

func BenchmarkSelectForInjection(b *testing.B) {
	mems := benchMemories(10)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = selectForInjection(mems, 0.6)
	}
}

func BenchmarkFormatContextBlock(b *testing.B) {
	mems := benchMemories(6)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = formatContextBlock(mems, 2048)
	}
}

func BenchmarkInjectContextAnthropic(b *testing.B) {
	block := "<retrieved-context source=\"muninn\">\n[c] (relevance: 0.90)\nmemory\n</retrieved-context>"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		doc := map[string]any{
			"model":    "claude-3",
			"system":   "You are helpful",
			"messages": []any{map[string]any{"role": "user", "content": "hello there"}},
		}
		if _, err := InjectContext(doc, apiformat.Anthropic, block); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFullInjectPipeline is the whole CPU cost a recall result incurs
// before forwarding: merge into the window, select, format, inject.
func BenchmarkFullInjectPipeline(b *testing.B) {
	inj := New(Config{MCPURL: "http://unused", Vault: "v"})
	recalled := benchMemories(10)
	block := "<retrieved-context source=\"muninn\">x</retrieved-context>"
	_ = block
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		merged := selectForInjection(inj.mergeMemories(recalled), inj.minScore)
		blk, _ := formatContextBlock(merged, inj.budget)
		doc := map[string]any{"system": "s", "messages": []any{map[string]any{"role": "user", "content": "q"}}}
		if _, err := InjectContext(doc, apiformat.Anthropic, blk); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCalibrateThreshold(b *testing.B) {
	scores := make([]float64, 200)
	for i := range scores {
		if i%2 == 0 {
			scores[i] = 0.45
		} else {
			scores[i] = 0.72
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = CalibrateThreshold(scores)
	}
}
