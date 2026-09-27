// This file contains the MuninnDB MCP wire path: the tool calls the injector
// makes (recall, guide, where_left_off) and the parsing of their JSON-RPC
// responses into memories and formatted context strings.
package inject

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
)

// fetchWhereLeftOff calls MuninnDB's muninn_where_left_off tool to get
// context from the previous session. Returns a formatted string for
// injection, or "" on failure or empty results.
func (inj *Injector) fetchWhereLeftOff(ctx context.Context) string {
	respBody, err := inj.mcp.Call(ctx, "muninn_where_left_off", map[string]any{
		"vault": inj.vault,
		"limit": 5, // 5 recent memories is enough to resume without overwhelming the system prompt
	})
	if err != nil {
		slog.Warn("where_left_off: call failed", "vault", inj.vault, "err", err)
		return ""
	}

	return parseWhereLeftOff(respBody)
}

// fetchGuide calls MuninnDB's muninn_guide tool to get global guidelines.
// Returns a formatted string for injection, or "" on failure or empty results.
func (inj *Injector) fetchGuide(ctx context.Context) string {
	respBody, err := inj.mcp.Call(ctx, "muninn_guide", map[string]any{
		"vault": inj.vault,
	})
	if err != nil {
		slog.Warn("guide: call failed", "vault", inj.vault, "err", err)
		return ""
	}

	return parseGuide(respBody)
}

// mcpResponse is the JSON-RPC envelope every MuninnDB tool replies in. The
// tool's own payload is carried as text inside a content block, not as the
// result value itself.
type mcpResponse struct {
	Result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
}

// parseMCPTextContent extracts the text from the first text-typed content block
// in a JSON-RPC response. A body that does not parse is a broken response, not
// an empty one, so the cause is logged (never the body: it carries memory
// content) before returning "" — otherwise a corrupt reply is indistinguishable
// from a server with no session context.
func parseMCPTextContent(body []byte) string {
	var rpcResp mcpResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		slog.Warn("inject: MCP response is not valid JSON; treating as empty", "err", err, "bytes", len(body))
		return ""
	}
	for _, c := range rpcResp.Result.Content {
		if c.Type == "text" && c.Text != "" {
			return c.Text
		}
	}
	return ""
}

// parseGuide extracts the guide text from the JSON-RPC response.
//
// The guide is free-form text from the memory backend, and every other
// injected block is prefixed with the data-not-instructions notice, so it gets
// the same treatment here: neutralize the block markers (a guide containing
// "</global-guide>" would close its own block and promote the rest to
// top-level system prompt) and state what the text is.
//
// The text is also capped (maxGuideRunes). It arrives as a single free-form
// string from the memory backend and is injected outside the per-memory token
// budget, so nothing downstream bounds it: a bloated or hostile guide would
// otherwise spend unbounded tokens on the system prompt of every session. The
// cap matches the bound the where_left_off fallback applies to its own
// free-form text below.
func parseGuide(body []byte) string {
	text := strings.TrimSpace(parseMCPTextContent(body))
	if text == "" {
		return ""
	}
	return apiformat.GlobalGuideOpen + "\n" + apiformat.ContextNotice + "\n" +
		apiformat.NeutralizeMarkers(apiformat.TruncateText(text, maxGuideRunes)) + "\n" +
		apiformat.GlobalGuideClose
}

// parseWhereLeftOff extracts a summary from the where_left_off JSON-RPC response.
func parseWhereLeftOff(body []byte) string {
	raw := parseMCPTextContent(body)
	if raw == "" {
		return ""
	}

	// Parse the inner result to extract memory summaries.
	var wloResult struct {
		Memories []struct {
			Concept string `json:"concept"`
			Summary string `json:"summary"`
		} `json:"memories"`
	}
	if err := json.Unmarshal([]byte(raw), &wloResult); err != nil {
		// The raw-text fallback is for a backend that answers with prose, not
		// JSON. A payload that IS JSON but has the wrong shape means the
		// protocol moved under us; injecting the raw response as "previous
		// session context" would put unvetted backend text in the system prompt
		// with no signal, so that case is dropped and logged.
		if json.Valid([]byte(raw)) {
			slog.Warn("inject: where_left_off payload is JSON with an unexpected shape, skipping",
				"bytes", len(raw), "err", err)
			return ""
		}
		// Not JSON, use the raw text if it's meaningful.
		text := strings.TrimSpace(raw)
		if text != "" && text != "[]" && text != "null" {
			return apiformat.SessionContextOpen + "\n" + apiformat.ContextNotice + "\nPrevious session context:\n" +
				apiformat.NeutralizeMarkers(apiformat.TruncateText(text, maxGuideRunes)) + "\n" + apiformat.SessionContextClose
		}
		return ""
	}

	if len(wloResult.Memories) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString(apiformat.SessionContextOpen + "\n" + apiformat.ContextNotice + "\nPrevious session context:\n")
	// Bound the number of entries: where_left_off is server-controlled and
	// otherwise unbounded by count, and this block is prepended outside the
	// per-memory budget packing. Capping entries (each already ≤200 runes) bounds
	// the block at the source while keeping it well-formed — preferable to
	// truncating the assembled string, which would cut the closing marker.
	shown := 0
	for _, m := range wloResult.Memories {
		if shown >= maxWhereLeftOffEntries {
			break
		}
		label := m.Concept
		if label == "" {
			label = m.Summary
		}
		if label != "" {
			sb.WriteString("- ")
			sb.WriteString(apiformat.NeutralizeMarkers(apiformat.TruncateText(label, maxWhereLeftOffLabelRunes)))
			sb.WriteString("\n")
			shown++
		}
	}
	sb.WriteString(apiformat.SessionContextClose)
	return sb.String()
}

// recall calls MuninnDB's muninn_recall tool via JSON-RPC. Returns at most 10
// memories above the configured relevance threshold. 10 gives the budget
// formatter enough candidates to fill the token budget without fetching
// more than will ever be injected.
func (inj *Injector) recall(ctx context.Context, query string) ([]memory, error) {
	args := map[string]any{
		"vault":     inj.vault,
		"context":   []string{query},
		"limit":     10,
		"threshold": inj.threshold,
		// Ask MuninnDB to annotate staleness: a memory whose fact has aged out is
		// flagged stale=true, an authoritative signal used to break same-concept
		// duplicates toward the current version (selectForInjection), stronger than
		// the created_at heuristic alone.
		"annotate": true,
	}
	if inj.recallMode != "" {
		args["mode"] = inj.recallMode // semantic: best retrieval (see cmd/msc-bench)
	}
	respBody, err := inj.mcp.Call(ctx, "muninn_recall", args)
	if err != nil {
		return nil, fmt.Errorf("recall request failed: %w", err)
	}

	mems, err := parseRecallResponse(respBody)
	if err != nil {
		return nil, err
	}
	// Gate/rank on cosine, not the composite score. If the server returns no
	// cosine at all, the gate falls back to the recency/graph-inflated composite,
	// which is unreliable for thresholding — warn once so operators can switch to
	// a recall mode/version that returns vector_score.
	if !normalizeRelevance(mems) && len(mems) > 0 {
		inj.noVectorWarn.Do(func() {
			slog.Warn("inject: recall returned no vector_score; gating on the composite score is unreliable — auto-calibration and the threshold may misbehave",
				"vault", inj.vault, "mode", inj.recallMode)
		})
	}
	return mems, nil
}

// parseRecallResponse extracts memories from a JSON-RPC response.
// JSON-RPC protocol errors are handled by mcpclient.Client.Call before
// this function is called, so this function only receives success responses.
func parseRecallResponse(body []byte) ([]memory, error) {
	var rpcResp mcpResponse

	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("parse JSON-RPC response: %w", err)
	}

	// The recall payload is JSON text inside a content block; the first text
	// block that parses wins. A server may prepend a human-readable summary
	// block, so a block that does not parse is skipped rather than fatal: one
	// unparsable block would otherwise abandon the memories in the next one.
	var firstErr error
	for _, content := range rpcResp.Result.Content {
		if content.Type != "text" {
			continue
		}

		// Try object format: {"memories": [...]} or {"results": [...]}.
		var recallResult struct {
			Memories []memory `json:"memories"`
			Results  []memory `json:"results"`
		}
		if err := json.Unmarshal([]byte(content.Text), &recallResult); err != nil {
			// Try parsing as a direct array.
			var direct []memory
			if err2 := json.Unmarshal([]byte(content.Text), &direct); err2 != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("parse recall result (struct: %v, array: %w)", err, err2)
				}
				continue
			}
			return direct, nil
		}

		if len(recallResult.Memories) > 0 {
			return recallResult.Memories, nil
		}
		if len(recallResult.Results) > 0 {
			return recallResult.Results, nil
		}
	}

	// Every text block was unparsable: a corrupt reply, not an empty result.
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, nil
}
