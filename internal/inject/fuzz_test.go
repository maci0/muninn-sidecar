package inject

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
)

// Fuzz targets for the inject parsing/selection surface: MuninnDB response
// parsers (untrusted server output) and the format-specific injection that
// rewrites untrusted request bodies. Invariant: never panic; when InjectContext
// reports success, its output must be valid JSON.

func FuzzParseRecallResponse(f *testing.F) {
	f.Add([]byte(`{"result":{"content":[{"type":"text","text":"{\"memories\":[{\"id\":\"1\",\"concept\":\"c\",\"content\":\"x\",\"score\":0.9,\"vector_score\":0.8}]}"}]}}`))
	f.Add([]byte(`{"result":{"content":[{"type":"text","text":"[]"}]}}`))
	f.Add([]byte(`{"result":{"content":[{"type":"text","text":"garbage"}]}}`))
	f.Add([]byte(`not json`))
	f.Fuzz(func(t *testing.T, data []byte) {
		mems, err := parseRecallResponse(data)
		if err == nil {
			normalizeRelevance(mems) // must tolerate whatever parsed
		}
	})
}

func FuzzParseWhereLeftOff(f *testing.F) {
	f.Add([]byte(`{"result":{"content":[{"type":"text","text":"{\"memories\":[{\"concept\":\"c\",\"summary\":\"s\"}]}"}]}}`))
	f.Add([]byte(`{"result":{"content":[{"type":"text","text":"raw summary text"}]}}`))
	f.Add([]byte(`{"result":{"content":[{"type":"text","text":"null"}]}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = parseWhereLeftOff(data)
	})
}

func FuzzParseGuide(f *testing.F) {
	f.Add([]byte(`{"result":{"content":[{"type":"text","text":"a guide"}]}}`))
	f.Add([]byte(`{"result":{"content":[]}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = parseGuide(data)
		_ = parseMCPTextContent(data)
	})
}

var fuzzFormats = []string{
	apiformat.Anthropic, apiformat.OpenAI, apiformat.OpenAIResponses,
	apiformat.Gemini, apiformat.GeminiCloudCode, "bogus",
}

func FuzzInjectContext(f *testing.F) {
	f.Add([]byte(`{"system":"x","messages":[]}`), 0, "ctx")
	f.Add([]byte(`{"messages":[{"role":"system","content":"s"}]}`), 1, "ctx")
	f.Add([]byte(`{"contents":[]}`), 3, "ctx")
	f.Add([]byte(`{"request":{"contents":[]}}`), 4, "ctx")
	f.Add([]byte(`{"input":"hi"}`), 2, "ctx")
	f.Fuzz(func(t *testing.T, data []byte, fi int, block string) {
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil || doc == nil {
			return
		}
		format := fuzzFormats[((fi%len(fuzzFormats))+len(fuzzFormats))%len(fuzzFormats)]
		out, err := InjectContext(doc, format, block)
		if err != nil {
			return
		}
		var check map[string]any
		if jerr := json.Unmarshal(out, &check); jerr != nil {
			t.Fatalf("InjectContext returned invalid JSON for format %q: %v", format, jerr)
		}
	})
}

func FuzzSelectAndFormat(f *testing.F) {
	f.Add("a|x|0.9\nb|y|0.7\nb|y2|0.65", 2048, 0.6)
	f.Add("only|one|0.95", 50, 0.6)
	f.Add("", 0, 0.6)
	f.Fuzz(func(t *testing.T, spec string, budget int, minScore float64) {
		if math.IsNaN(minScore) || math.IsInf(minScore, 0) {
			return
		}
		// Build memories from "concept|content|score" lines.
		var mems []memory
		for _, line := range strings.Split(spec, "\n") {
			parts := strings.Split(line, "|")
			if len(parts) < 3 {
				continue
			}
			score := 0.0
			if err := json.Unmarshal([]byte(parts[2]), &score); err != nil {
				score = float64(len(parts[2])%100) / 100
			}
			mems = append(mems, memory{Concept: parts[0], Content: parts[1], Score: score})
		}
		sel := selectForInjection(mems, minScore)
		kept := withinBudget(sel, budget)
		block, tokens, _ := formatContextBlock(kept, budget)
		if tokens < 0 {
			t.Fatalf("negative token count %d", tokens)
		}
		if len(kept) > 0 && block == "" {
			t.Fatalf("non-empty selection produced empty block")
		}
	})
}

func FuzzMetricPrimitives(f *testing.F) {
	f.Add(2, 4, 5)
	f.Fuzz(func(t *testing.T, rel, inj, total int) {
		// These are counts (always >= 0 in production); fuzz the realistic domain.
		abs := func(x int) int {
			if x < 0 {
				return -x
			}
			return x
		}
		rel, inj, total = abs(rel)%100000, abs(inj)%100000, abs(total)%100000
		// injected-relevant can't exceed the injected count nor the total relevant.
		if rel > inj {
			rel = inj
		}
		if rel > total {
			rel = total
		}
		p := precision(rel, inj)
		r := recall(rel, total)
		_ = f1(p, r)
		_ = wastedRatio(rel, inj)
		if p < 0 || p > 1.0001 || r < 0 || r > 1.0001 {
			t.Fatalf("metric out of range p=%v r=%v", p, r)
		}
	})
}

func FuzzCalibrateThreshold(f *testing.F) {
	f.Add([]byte{200, 120, 60, 30})
	f.Fuzz(func(t *testing.T, data []byte) {
		// Interpret bytes as scores in [0,1].
		scores := make([]float64, len(data))
		for i, b := range data {
			scores[i] = float64(b) / 255
		}
		got := CalibrateThreshold(scores)
		if got < 0 || got > 1 {
			t.Fatalf("threshold out of range: %v", got)
		}
	})
}

func FuzzNDCGFuzz(f *testing.F) {
	f.Add([]byte{1, 0, 1, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		rels := make([]int, len(data))
		for i, b := range data {
			rels[i] = int(b & 1)
		}
		if v := ndcg(rels); v < 0 || v > 1.0001 {
			t.Fatalf("ndcg out of range: %v", v)
		}
	})
}
