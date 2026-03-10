package inject

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
	"github.com/maci0/muninn-sidecar/internal/stats"
)

// Config holds parameters for creating an Injector.
type Config struct {
	MCPURL    string        // MuninnDB MCP endpoint
	Token     string        // Bearer token for auth
	Vault     string        // vault to recall from (default: "sidecar")
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

	// Session-start: call where_left_off and guide once on first enrichment.
	sessionOnce sync.Once
	sessionCtx  string // cached where_left_off context block, empty if none
	sessionMu   sync.Mutex
}

// New creates an Injector with the given configuration.
func New(cfg Config) *Injector {
	if cfg.Vault == "" {
		cfg.Vault = "sidecar"
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
//
// On the first call (session start), it also calls muninn_where_left_off and
// muninn_guide to provide continuity from the previous session and global guidelines.
func (inj *Injector) Enrich(ctx context.Context, body []byte) ([]byte, int, error) {
	// On first call, fetch where_left_off context and guide concurrently (best-effort).
	inj.sessionOnce.Do(func() {
		var wg sync.WaitGroup
		var wlo, guide string

		wg.Add(2)
		go func() {
			defer wg.Done()
			ctxW, cancelW := context.WithTimeout(ctx, inj.timeout)
			defer cancelW()
			wlo = inj.fetchWhereLeftOff(ctxW)
		}()

		go func() {
			defer wg.Done()
			ctxG, cancelG := context.WithTimeout(ctx, inj.timeout)
			defer cancelG()
			guide = inj.fetchGuide(ctxG)
		}()

		wg.Wait()

		var sb strings.Builder
		if wlo != "" {
			sb.WriteString(wlo)
		}
		if guide != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(guide)
		}
		inj.sessionMu.Lock()
		inj.sessionCtx = sb.String()
		inj.sessionMu.Unlock()
	})

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, 0, nil // not JSON, pass through
	}

	format := apiformat.DetectFormat(doc)
	if format == "" {
		keys := make([]string, 0, len(doc))
		for k := range doc {
			keys = append(keys, k)
		}
		slog.Debug("inject: unknown request format, skipping", "keys", keys)
		return body, 0, nil // unknown format, pass through
	}

	// Extract the last 3 turns of conversation to provide a rich semantic query.
	query := apiformat.ExtractRecentContext(doc, format, 3)
	if query == "" {
		slog.Debug("inject: no user query found", "format", format)
		return body, 0, nil // no message to search with
	}

	slog.Debug("inject: recalling", "format", format, "query_len", len(query))

	query = apiformat.TruncateQuery(query, 2000)

	// Recall from MuninnDB with timeout.
	recallCtx, cancel := context.WithTimeout(ctx, inj.timeout)
	defer cancel()

	memories, err := inj.recall(recallCtx, query)
	if err != nil {
		slog.Debug("inject recall failed, passing through", "err", err)
		return body, 0, nil // graceful fallback
	}

	slog.Debug("inject: recall returned", "count", len(memories))

	inj.sessionMu.Lock()
	hasSessionCtx := inj.sessionCtx != ""
	inj.sessionMu.Unlock()

	if len(memories) == 0 && !hasSessionCtx {
		return body, 0, nil
	}

	// Format context block within token budget.
	block, tokens := formatContextBlock(memories, inj.budget)

	// Prepend where_left_off context on first enrichment.
	inj.sessionMu.Lock()
	sessCtx := inj.sessionCtx
	inj.sessionCtx = "" // only inject once
	inj.sessionMu.Unlock()

	if sessCtx != "" {
		block = sessCtx + "\n" + block
		tokens += len(sessCtx) / 4
	}

	block = strings.TrimSpace(block)
	if block == "" {
		return body, 0, nil
	}

	// Inject into the document.
	enriched, err := InjectContext(doc, format, block)
	if err != nil {
		slog.Debug("inject context failed", "err", err)
		return body, 0, nil
	}

	// Update stats.
	if inj.stats != nil {
		inj.stats.Injections.Add(1)
		inj.stats.InjectedTokens.Add(int64(tokens))
	}

	return enriched, tokens, nil
}

// doMCPCall makes a JSON-RPC 2.0 tool call to the MuninnDB MCP server.
func (inj *Injector) doMCPCall(ctx context.Context, toolName string, args map[string]any) ([]byte, error) {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
		"id": time.Now().UnixNano(),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", inj.mcpURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if inj.token != "" {
		req.Header.Set("Authorization", "Bearer "+inj.token)
	}

	resp, err := inj.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return respBody, nil
}

// fetchWhereLeftOff calls MuninnDB's muninn_where_left_off tool to get
// context from the previous session. Returns a formatted string for
// injection, or "" on failure or empty results.
func (inj *Injector) fetchWhereLeftOff(ctx context.Context) string {
	respBody, err := inj.doMCPCall(ctx, "muninn_where_left_off", map[string]any{
		"vault": inj.vault,
		"limit": 5,
	})
	if err != nil {
		slog.Debug("where_left_off: call failed", "err", err)
		return ""
	}

	return parseWhereLeftOff(respBody)
}

// fetchGuide calls MuninnDB's muninn_guide tool to get global guidelines.
func (inj *Injector) fetchGuide(ctx context.Context) string {
	respBody, err := inj.doMCPCall(ctx, "muninn_guide", map[string]any{
		"vault": inj.vault,
	})
	if err != nil {
		slog.Debug("guide: call failed", "err", err)
		return ""
	}

	return parseGuide(respBody)
}

// parseGuide extracts the guide text from the JSON-RPC response.
func parseGuide(body []byte) string {
	var rpcResp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}

	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return ""
	}

	for _, content := range rpcResp.Result.Content {
		if content.Type != "text" || content.Text == "" {
			continue
		}
		
		// Typically guide just returns a string wrapped in text.
		text := strings.TrimSpace(content.Text)
		if text != "" {
			return "<global-guide source=\"muninn\">\n" + text + "\n</global-guide>"
		}
	}
	return ""
}

// parseWhereLeftOff extracts a summary from the where_left_off JSON-RPC response.
func parseWhereLeftOff(body []byte) string {
	var rpcResp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}

	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return ""
	}

	for _, content := range rpcResp.Result.Content {
		if content.Type != "text" || content.Text == "" {
			continue
		}

		// Parse the inner result to extract memory summaries.
		var wloResult struct {
			Memories []struct {
				Concept string `json:"concept"`
				Summary string `json:"summary"`
			} `json:"memories"`
		}
		if err := json.Unmarshal([]byte(content.Text), &wloResult); err != nil {
			// If not JSON, use the raw text if it's meaningful.
			text := strings.TrimSpace(content.Text)
			if text != "" && text != "[]" && text != "null" {
				return "<session-context source=\"muninn\">\nPrevious session context:\n" + apiformat.TruncateText(text, 2000) + "\n</session-context>"
			}
			return ""
		}

		if len(wloResult.Memories) == 0 {
			return ""
		}

		var sb strings.Builder
		sb.WriteString("<session-context source=\"muninn\">\nPrevious session context:\n")
		for _, m := range wloResult.Memories {
			label := m.Concept
			if label == "" {
				label = m.Summary
			}
			if label != "" {
				sb.WriteString("- ")
				sb.WriteString(apiformat.TruncateText(label, 200))
				sb.WriteString("\n")
			}
		}
		sb.WriteString("</session-context>")
		return sb.String()
	}

	return ""
}

// recall calls MuninnDB's muninn_recall tool via JSON-RPC.
func (inj *Injector) recall(ctx context.Context, query string) ([]memory, error) {
	respBody, err := inj.doMCPCall(ctx, "muninn_recall", map[string]any{
		"vault":     inj.vault,
		"context":   []string{query},
		"limit":     10,
		"threshold": inj.threshold,
	})
	if err != nil {
		return nil, fmt.Errorf("recall request failed: %w", err)
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
