package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"unicode"

	"github.com/maci0/muninn-sidecar/internal/mcpclient"
)

// --- recall (mirrors the proxy's gated selection: cosine >= minScore) ---

// splitQueryQA decomposes a query into sub-queries (full + capitalized entity
// spans) for transparent multi-recall — no LLM call.
func splitQueryQA(q string) []string {
	subs := []string{q}
	seen := map[string]bool{q: true}
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			s := strings.Trim(strings.Join(cur, " "), "?.,")
			if len(s) >= 3 && !seen[s] {
				seen[s] = true
				subs = append(subs, s)
			}
			cur = nil
		}
	}
	for _, w := range strings.Fields(q) {
		// unicode.IsUpper, not an 'A'..'Z' range test: a German noun, a Turkish
		// or Cyrillic proper noun, and any CJK entity carry no ASCII uppercase,
		// so the range test found no sub-queries for those questions.
		r := []rune(w)
		if len(r) > 0 && unicode.IsUpper(r[0]) {
			cur = append(cur, w)
		} else {
			flush()
		}
	}
	flush()
	return subs
}

// cand is a gated recall candidate with the metadata the production injection
// format carries (concept + effective cosine), so the harness can reproduce the
// real `[concept] (relevance: X.XX)\ncontent` block, not just bare content.
type cand struct {
	Concept string
	Content string
	Score   float64 // effective cosine used as the relevance shown to the reader
}

func recallContext(ctx context.Context, mcp *mcpclient.Client, vault, query string, minScore float64, multi bool) string {
	return strings.Join(recallCandidates(ctx, mcp, vault, query, minScore, multi), "\n")
}

// recallCandidates returns the gated recall passages' content in recall order
// (bare content — used for grounding and the distractor arm).
func recallCandidates(ctx context.Context, mcp *mcpclient.Client, vault, query string, minScore float64, multi bool) []string {
	cands := recallStructured(ctx, mcp, vault, query, minScore, multi)
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Content
	}
	return out
}

// recallStructured returns the gated recall candidates (cosine >= minScore) with
// concept and relevance, in MuninnDB's return order (already score-ranked; the
// multi-query path concatenates per-sub-query results, not a global ranking).
func recallStructured(ctx context.Context, mcp *mcpclient.Client, vault, query string, minScore float64, multi bool) []cand {
	if multi {
		// Dedup by content, keeping the best score across sub-queries: a memory
		// scoring low vs the full question but high vs an entity sub-query must
		// carry the high score, or a downstream gate would wrongly reject it.
		seen := map[string]int{}
		var parts []cand
		for _, sub := range splitQueryQA(query) {
			for _, c := range recallStructured(ctx, mcp, vault, sub, minScore, false) {
				if c.Content == "" {
					continue
				}
				if j, ok := seen[c.Content]; ok {
					if c.Score > parts[j].Score {
						parts[j] = c
					}
					continue
				}
				seen[c.Content] = len(parts)
				parts = append(parts, c)
			}
		}
		return parts
	}
	resp, err := mcp.Call(ctx, "muninn_recall", map[string]any{
		"vault": vault, "context": []string{query}, "limit": 5, "threshold": 0.05, "mode": "semantic",
	})
	if err != nil {
		return nil
	}
	var rpc struct {
		Result struct {
			Content []struct {
				Type, Text string
			} `json:"content"`
		} `json:"result"`
	}
	if json.Unmarshal(resp, &rpc) != nil {
		return nil
	}
	for _, c := range rpc.Result.Content {
		if c.Type != "text" {
			continue
		}
		var inner struct {
			Memories []struct {
				Concept     string  `json:"concept"`
				Content     string  `json:"content"`
				VectorScore float64 `json:"vector_score"`
				Score       float64 `json:"score"`
			} `json:"memories"`
		}
		if json.Unmarshal([]byte(c.Text), &inner) != nil {
			return nil
		}
		var parts []cand
		for _, m := range inner.Memories {
			rel := m.VectorScore
			if rel == 0 {
				rel = m.Score
			}
			if rel >= minScore { // the gate
				parts = append(parts, cand{Concept: m.Concept, Content: m.Content, Score: rel})
			}
		}
		return parts
	}
	return nil
}

// formatInjected renders gated candidates into the context body for a given
// presentation mode: "bare" joins raw content; "scored" reproduces the live
// proxy's per-entry format ("[concept] (relevance: X.XX)\ncontent"); "labeled"
// keeps the concept header but drops the relevance number.
func formatInjected(cands []cand, mode string) string {
	if len(cands) == 0 {
		return ""
	}
	switch mode {
	case "scored", "labeled":
		var sb strings.Builder
		for i, c := range cands {
			if i > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString("[" + c.Concept + "]")
			if mode == "scored" {
				sb.WriteString(" (relevance: " + strconv.FormatFloat(c.Score, 'f', 2, 64) + ")")
			}
			sb.WriteString("\n" + c.Content)
		}
		return sb.String()
	default: // "bare"
		parts := make([]string, len(cands))
		for i, c := range cands {
			parts[i] = c.Content
		}
		return strings.Join(parts, "\n")
	}
}
