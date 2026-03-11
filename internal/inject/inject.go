package inject

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
	"github.com/maci0/muninn-sidecar/internal/mcpclient"
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

// decayFactor is multiplied against a memory's score for each turn it is
// not re-recalled. This creates a gradual fade-out rather than abrupt removal.
const decayFactor = 0.7

// decayFloor is the minimum effective score before a memory is evicted from
// the session window. With decayFactor=0.7 and a starting score of 0.85,
// a memory survives ~4 turns without being re-recalled before falling below 0.2.
const decayFloor = 0.2

// trackedMemory wraps a recalled memory with session-level tracking state.
type trackedMemory struct {
	memory
	lastSeen int // turn number when last recalled/refreshed
}

// Injector enriches LLM API requests with recalled memories from MuninnDB.
// It maintains a session-level memory window: recalled memories persist across
// turns with decaying scores, so context from earlier turns fades gradually
// instead of vanishing when the next query's recall results differ.
type Injector struct {
	mcp       *mcpclient.Client
	vault     string
	budget    int
	threshold float64
	timeout   time.Duration
	stats     *stats.Stats

	// Session-start: call where_left_off and guide once on first enrichment.
	sessionOnce sync.Once
	sessionCtx  string // cached where_left_off context block, empty if none

	// Session memory window: rolling set of memories injected across turns.
	mu             sync.Mutex
	turn           int                      // monotonically increasing turn counter
	recentMemories map[string]trackedMemory // memory ID → tracked state
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
		mcp:            mcpclient.New(cfg.MCPURL, cfg.Token, cfg.Timeout),
		vault:          cfg.Vault,
		budget:         cfg.Budget,
		threshold:      cfg.Threshold,
		timeout:        cfg.Timeout,
		stats:          cfg.Stats,
		recentMemories: make(map[string]trackedMemory),
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
	// Use context.Background() instead of the request context because sync.Once
	// never retries — if the request context is cancelled (client disconnect,
	// timeout), the session initialization would be permanently lost.
	inj.sessionOnce.Do(func() {
		var wg sync.WaitGroup
		var wlo, guide string

		wg.Add(2)
		go func() {
			defer wg.Done()
			ctxW, cancelW := context.WithTimeout(context.Background(), inj.timeout)
			defer cancelW()
			wlo = inj.fetchWhereLeftOff(ctxW)
		}()

		go func() {
			defer wg.Done()
			ctxG, cancelG := context.WithTimeout(context.Background(), inj.timeout)
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
		inj.mu.Lock()
		inj.sessionCtx = sb.String()
		inj.mu.Unlock()
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

	// Merge recalled memories into the session window with decay.
	merged := inj.mergeMemories(memories)

	inj.mu.Lock()
	hasSessionCtx := inj.sessionCtx != ""
	inj.mu.Unlock()

	if len(merged) == 0 && !hasSessionCtx {
		return body, 0, nil
	}

	// Format context block within token budget.
	block, tokens := formatContextBlock(merged, inj.budget)

	// Prepend where_left_off context on first enrichment.
	inj.mu.Lock()
	sessCtx := inj.sessionCtx
	inj.mu.Unlock()

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

	// Clear session context only after successful injection to avoid
	// permanently losing it if InjectContext fails.
	if sessCtx != "" {
		inj.mu.Lock()
		inj.sessionCtx = ""
		inj.mu.Unlock()
	}

	// Update stats.
	if inj.stats != nil {
		inj.stats.Injections.Add(1)
		inj.stats.InjectedTokens.Add(int64(tokens))
	}

	return enriched, tokens, nil
}

// mergeMemories merges freshly recalled memories into the session window.
// Previously seen memories that are recalled again have their score refreshed.
// Memories not re-recalled decay by decayFactor per turn and are evicted when
// they drop below decayFloor. Returns the merged set sorted by effective score.
func (inj *Injector) mergeMemories(recalled []memory) []memory {
	inj.mu.Lock()
	defer inj.mu.Unlock()

	inj.turn++
	currentTurn := inj.turn

	// Refresh or add recalled memories.
	recalledIDs := make(map[string]bool, len(recalled))
	for _, m := range recalled {
		recalledIDs[m.ID] = true
		inj.recentMemories[m.ID] = trackedMemory{
			memory:   m,
			lastSeen: currentTurn,
		}
	}

	// Decay and evict stale memories.
	for id, tm := range inj.recentMemories {
		if recalledIDs[id] {
			continue // just refreshed
		}
		turnsAgo := currentTurn - tm.lastSeen
		effective := tm.Score * math.Pow(decayFactor, float64(turnsAgo))
		if effective < decayFloor {
			delete(inj.recentMemories, id)
			slog.Debug("inject: evicted stale memory", "id", id, "age", turnsAgo)
		}
	}

	// Build sorted output from the merged window.
	merged := make([]memory, 0, len(inj.recentMemories))
	for _, tm := range inj.recentMemories {
		turnsAgo := currentTurn - tm.lastSeen
		effective := tm.Score * math.Pow(decayFactor, float64(turnsAgo))
		merged = append(merged, memory{
			ID:      tm.ID,
			Concept: tm.Concept,
			Content: tm.Content,
			Score:   effective,
		})
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Score > merged[j].Score
	})

	return merged
}

// fetchWhereLeftOff calls MuninnDB's muninn_where_left_off tool to get
// context from the previous session. Returns a formatted string for
// injection, or "" on failure or empty results.
func (inj *Injector) fetchWhereLeftOff(ctx context.Context) string {
	respBody, err := inj.mcp.Call(ctx, "muninn_where_left_off", map[string]any{
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
	respBody, err := inj.mcp.Call(ctx, "muninn_guide", map[string]any{
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
	respBody, err := inj.mcp.Call(ctx, "muninn_recall", map[string]any{
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
