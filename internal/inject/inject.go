package inject

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/maci0/muninn-sidecar/internal/stats"
)

// Config holds parameters for creating an Injector.
type Config struct {
	MCPURL    string        // MuninnDB MCP endpoint
	Token     string        // Bearer token for auth
	Vault     string        // vault to recall from (default: "default")
	Budget    int           // max approximate tokens to inject (default: 2048)
	Threshold float64       // min relevance score (default: 0.4)
	Timeout   time.Duration // recall timeout (default: 200ms)
	Stats     *stats.Stats  // session statistics (nil-safe)
}

// Injector enriches LLM API requests with recalled memories from MuninnDB.
type Injector struct {
	mcpURL    string
	token     string
	vault     string
	budget    int
	threshold float64
	timeout   time.Duration
	stats     *stats.Stats
	client    *http.Client

	// Turn tracking: ring buffer of 5 turns, 2-turn cooldown.
	mu       sync.Mutex
	turnRing [5]map[string]bool
	turnIdx  int
}

// New creates an Injector with the given configuration.
func New(cfg Config) *Injector {
	if cfg.Vault == "" {
		cfg.Vault = "default"
	}
	if cfg.Budget <= 0 {
		cfg.Budget = 2048
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = 0.4
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 200 * time.Millisecond
	}

	return &Injector{
		mcpURL:    cfg.MCPURL,
		token:     cfg.Token,
		vault:     cfg.Vault,
		budget:    cfg.Budget,
		threshold: cfg.Threshold,
		timeout:   cfg.Timeout,
		stats:     cfg.Stats,
		client:    &http.Client{Timeout: cfg.Timeout},
	}
}

// Enrich parses a request body, recalls relevant memories, and injects them
// as system-level context. Returns the enriched body, estimated injected token
// count, and any error. On any failure, returns the original body unchanged.
func (inj *Injector) Enrich(ctx context.Context, body []byte) ([]byte, int, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, 0, nil // not JSON, pass through
	}

	format := detectFormat(doc)
	if format == "" {
		keys := make([]string, 0, len(doc))
		for k := range doc {
			keys = append(keys, k)
		}
		slog.Debug("inject: unknown request format, skipping", "keys", keys)
		return body, 0, nil // unknown format, pass through
	}

	query := extractUserQuery(doc, format)
	if query == "" {
		slog.Debug("inject: no user query found", "format", format)
		return body, 0, nil // no user message to search with
	}

	slog.Debug("inject: recalling", "format", format, "query_len", len(query))

	query = truncateQuery(query, 2000)

	// Recall from MuninnDB with timeout.
	recallCtx, cancel := context.WithTimeout(ctx, inj.timeout)
	defer cancel()

	memories, err := inj.recall(recallCtx, query)
	if err != nil {
		slog.Debug("inject recall failed, passing through", "err", err)
		return body, 0, nil // graceful fallback
	}

	slog.Debug("inject: recall returned", "count", len(memories))

	if len(memories) == 0 {
		return body, 0, nil
	}

	// Filter out recently injected memories (2-turn cooldown).
	memories = inj.filterRecent(memories)
	if len(memories) == 0 {
		slog.Debug("inject: all memories filtered by dedup")
		return body, 0, nil
	}

	// Format context block within token budget.
	block, tokens := formatContextBlock(memories, inj.budget)
	if block == "" {
		return body, 0, nil
	}

	// Inject into the document.
	enriched, err := injectContext(doc, format, block)
	if err != nil {
		slog.Debug("inject context failed", "err", err)
		return body, 0, nil
	}

	// Record injected IDs for turn tracking.
	var ids []string
	for _, m := range memories {
		ids = append(ids, m.ID)
	}
	inj.recordInjected(ids)

	// Update stats.
	if inj.stats != nil {
		inj.stats.Injections.Add(1)
		inj.stats.InjectedTokens.Add(int64(tokens))
	}

	return enriched, tokens, nil
}

// recall calls MuninnDB's muninn_recall tool via JSON-RPC.
func (inj *Injector) recall(ctx context.Context, query string) ([]memory, error) {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]any{
			"name": "muninn_recall",
			"arguments": map[string]any{
				"vault":     inj.vault,
				"context":   []string{query},
				"limit":     10,
				"threshold": inj.threshold,
			},
		},
		"id": time.Now().UnixNano(),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal recall request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", inj.mcpURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create recall request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if inj.token != "" {
		req.Header.Set("Authorization", "Bearer "+inj.token)
	}

	resp, err := inj.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("recall request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read recall response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("recall returned HTTP %d", resp.StatusCode)
	}

	return parseRecallResponse(respBody)
}

// parseRecallResponse extracts memories from a JSON-RPC response.
// The MCP response wraps the tool result in result.content[].text.
func parseRecallResponse(body []byte) ([]memory, error) {
	var rpcResp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("parse JSON-RPC response: %w", err)
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("JSON-RPC error: %s", rpcResp.Error.Message)
	}

	// The recall tool returns its result as JSON text inside content[0].text.
	for _, content := range rpcResp.Result.Content {
		if content.Type != "text" {
			continue
		}

		var recallResult struct {
			Memories []struct {
				ID      string  `json:"id"`
				Concept string  `json:"concept"`
				Content string  `json:"content"`
				Score   float64 `json:"score"`
			} `json:"memories"`
			// Alternative flat format.
			Results []struct {
				ID      string  `json:"id"`
				Concept string  `json:"concept"`
				Content string  `json:"content"`
				Score   float64 `json:"score"`
			} `json:"results"`
		}

		if err := json.Unmarshal([]byte(content.Text), &recallResult); err != nil {
			// Try parsing as a direct array.
			var direct []struct {
				ID      string  `json:"id"`
				Concept string  `json:"concept"`
				Content string  `json:"content"`
				Score   float64 `json:"score"`
			}
			if err2 := json.Unmarshal([]byte(content.Text), &direct); err2 != nil {
				return nil, fmt.Errorf("parse recall result: %w", err)
			}
			var mems []memory
			for _, d := range direct {
				mems = append(mems, memory{ID: d.ID, Concept: d.Concept, Content: d.Content, Score: d.Score})
			}
			return mems, nil
		}

		items := recallResult.Memories
		if len(items) == 0 {
			items = recallResult.Results
		}

		var mems []memory
		for _, item := range items {
			mems = append(mems, memory{
				ID:      item.ID,
				Concept: item.Concept,
				Content: item.Content,
				Score:   item.Score,
			})
		}
		return mems, nil
	}

	return nil, nil
}

// filterRecent removes memories that were injected in the last 2 turns.
func (inj *Injector) filterRecent(mems []memory) []memory {
	inj.mu.Lock()
	defer inj.mu.Unlock()

	var filtered []memory
	for _, m := range mems {
		if inj.seenRecently(m.ID) {
			continue
		}
		filtered = append(filtered, m)
	}
	return filtered
}

// seenRecently checks if an ID was injected in the last 2 turn slots.
// Must be called with mu held.
func (inj *Injector) seenRecently(id string) bool {
	// Check current slot and previous slot (2-turn cooldown).
	for offset := 0; offset < 2; offset++ {
		idx := (inj.turnIdx - offset + len(inj.turnRing)) % len(inj.turnRing)
		if inj.turnRing[idx] != nil && inj.turnRing[idx][id] {
			return true
		}
	}
	return false
}

// recordInjected advances the turn counter and records IDs in the new slot.
func (inj *Injector) recordInjected(ids []string) {
	inj.mu.Lock()
	defer inj.mu.Unlock()

	inj.turnIdx = (inj.turnIdx + 1) % len(inj.turnRing)
	inj.turnRing[inj.turnIdx] = make(map[string]bool, len(ids))
	for _, id := range ids {
		inj.turnRing[inj.turnIdx][id] = true
	}
}
