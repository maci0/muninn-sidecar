package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// recallStructured returns the gated recall candidates (cosine >= minScore) with
// concept and relevance, in MuninnDB's return order (already score-ranked; the
// multi-query path concatenates per-sub-query results, not a global ranking).
//
// A transport failure is returned rather than folded into an empty result: the
// arms scored on an empty injected context would look like a real (zero) answer
// instead of a run that never reached MuninnDB. The multi-query path reports the
// first failing sub-query and still returns what the others recalled.
func recallStructured(ctx context.Context, mcp *mcpclient.Client, vault, query string, minScore float64, multi bool) ([]cand, error) {
	if multi {
		// Dedup by content, keeping the best score across sub-queries: a memory
		// scoring low vs the full question but high vs an entity sub-query must
		// carry the high score, or a downstream gate would wrongly reject it.
		seen := map[string]int{}
		var parts []cand
		var firstErr error
		for _, sub := range querysplit.Split(query) {
			cands, err := recallStructured(ctx, mcp, vault, sub, minScore, false)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			for _, c := range cands {
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
		return parts, firstErr
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
	return parseRecallPayload(resp, minScore)
}

// parseRecallPayload turns a raw muninn_recall JSON-RPC reply into the gated
// candidates. Split out of recallStructured so the parse is reachable without a
// live MCP server: this is server-controlled JSON, decoded twice (the envelope,
// then the text content block's payload), and every one of those numbers is a
// float the gate and formatInjected print verbatim.
//
// A reply that does not decode is an error, not an empty result. This harness
// exists to measure, and a broken envelope read as "no memories" turns a
// transport fault into answer-coverage 0/100 reported as a real score. A reply
// that decodes and carries no memories is a genuine empty result and returns
// nil with no error.
func parseRecallPayload(resp []byte, minScore float64) ([]cand, error) {
	var rpc struct {
		Result struct {
			Content []struct {
				Type, Text string
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &rpc); err != nil {
		return nil, fmt.Errorf("parse recall envelope (%d bytes): %w", len(resp), err)
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
		if err := json.Unmarshal([]byte(c.Text), &inner); err != nil {
			return nil, fmt.Errorf("parse recall payload (%d bytes): %w", len(c.Text), err)
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
		return parts, nil
	}
	return nil, nil
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
