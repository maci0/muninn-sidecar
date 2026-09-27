package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/maci0/muninn-sidecar/internal/mcpclient"
	"github.com/maci0/muninn-sidecar/internal/querysplit"
)

// --- recall (mirrors the proxy's gated selection: cosine >= minScore) ---

// cand is a gated recall candidate with the metadata the production injection
// format carries (concept + effective cosine), so the harness can reproduce the
// real `[concept] (relevance: X.XX)\ncontent` block, not just bare content.
type cand struct {
	Concept string
	Content string
	Score   float64 // effective cosine used as the relevance shown to the reader
}

func recallContext(ctx context.Context, mcp *mcpclient.Client, vault, query string, minScore float64, multi bool) string {
	cands, err := recallCandidates(ctx, mcp, vault, query, minScore, multi)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warn: recall for context %q failed: %v\n", query, err)
		return ""
	}
	return strings.Join(cands, "\n")
}

// recallCandidates returns the gated recall passages' content in recall order
// (bare content — used for grounding and the distractor arm).
func recallCandidates(ctx context.Context, mcp *mcpclient.Client, vault, query string, minScore float64, multi bool) ([]string, error) {
	cands, err := recallStructured(ctx, mcp, vault, query, minScore, multi)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Content
	}
	return out, nil
}

// recallStructured returns the gated recall candidates (cosine >= minScore) with
// concept and relevance, in MuninnDB's return order (already score-ranked; the
// multi-query path concatenates per-sub-query results, not a global ranking).
func recallStructured(ctx context.Context, mcp *mcpclient.Client, vault, query string, minScore float64, multi bool) ([]cand, error) {
	if multi {
		// Dedup by content, keeping the best score across sub-queries: a memory
		// scoring low vs the full question but high vs an entity sub-query must
		// carry the high score, or a downstream gate would wrongly reject it.
		seen := map[string]int{}
		var parts []cand
		for _, sub := range querysplit.Split(query) {
			cs, err := recallStructured(ctx, mcp, vault, sub, minScore, false)
			if err != nil {
				return nil, err
			}
			for _, c := range cs {
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
		return parts, nil
	}
	resp, err := mcp.Call(ctx, "muninn_recall", map[string]any{
		"vault": vault, "context": []string{query}, "limit": 5, "threshold": 0.05, "mode": "semantic",
	})
	if err != nil {
		// An empty candidate set is the QA harness's worst outcome: every arm is
		// built from it and the run reports answer-coverage 0/100 as a
		// measurement. Surface the failure instead.
		return nil, fmt.Errorf("recall %q from vault %q: %w", query, vault, err)
	}
	return parseRecallPayload(resp, minScore), nil
}

// parseRecallPayload turns a raw muninn_recall JSON-RPC reply into the gated
// candidates. Split out of recallStructured so the parse is reachable without a
// live MCP server: this is server-controlled JSON, decoded twice (the envelope,
// then the text content block's payload), and every one of those numbers is a
// float the gate and formatInjected print verbatim.
func parseRecallPayload(resp []byte, minScore float64) []cand {
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
