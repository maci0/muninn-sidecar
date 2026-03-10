// Package store provides async delivery of captured API exchanges to
// MuninnDB via MCP JSON-RPC.
package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/maci0/muninn-sidecar/internal/stats"
)

// MuninnStore delivers captured API exchanges to MuninnDB via MCP JSON-RPC.
// Writes are async: Store() enqueues to a buffered channel, and a background
// goroutine batches them (up to 10 per call, flushed every 2s) using
// muninn_remember_batch. This keeps the proxy's hot path free of network I/O.
type MuninnStore struct {
	mcpURL string       // MCP endpoint, e.g. http://127.0.0.1:8750/mcp
	token  string       // Bearer token for MuninnDB auth; empty = no auth
	vault  string       // target vault in MuninnDB (default: "sidecar")
	client *http.Client // dedicated HTTP client with short timeout
	queue  chan *CapturedExchange
	done   chan struct{} // closed when Drain completes
	stats  *stats.Stats // session statistics (nil-safe)
}

// CapturedExchange holds one request->response pair captured by the proxy.
type CapturedExchange struct {
	Timestamp  time.Time       `json:"timestamp"`
	Agent      string          `json:"agent"`                  // which coding agent (claude, gemini, etc.)
	Method     string          `json:"method"`                 // HTTP method
	Path       string          `json:"path"`                   // request path (e.g. /v1/messages)
	ReqBody    json.RawMessage `json:"req_body,omitempty"`
	StatusCode int             `json:"status_code"`
	RespBody   json.RawMessage `json:"resp_body,omitempty"`
	DurationMs int64           `json:"duration_ms"`
	Model      string          `json:"model,omitempty"`        // extracted from req/resp JSON
	TokensIn   int             `json:"tokens_in,omitempty"`    // input/prompt token count
	TokensOut  int             `json:"tokens_out,omitempty"`   // output/completion token count
	CacheWrite int             `json:"cache_write,omitempty"`  // Anthropic cache_creation_input_tokens
	CacheRead  int             `json:"cache_read,omitempty"`   // Anthropic cache_read_input_tokens
}

// New creates a MuninnStore and starts its background flush goroutine.
// The queue depth of 256 provides back-pressure: if MuninnDB is unreachable
// for an extended period, new captures are dropped rather than letting
// memory grow unbounded. Pass a non-nil Stats to track session metrics.
func New(mcpURL, token, vault string, st *stats.Stats) *MuninnStore {
	if vault == "" {
		vault = "sidecar"
	}
	s := &MuninnStore{
		mcpURL: strings.TrimRight(mcpURL, "/"),
		token:  token,
		vault:  vault,
		client: &http.Client{Timeout: 10 * time.Second},
		queue:  make(chan *CapturedExchange, 256),
		done:   make(chan struct{}),
		stats:  st,
	}
	go s.worker()
	return s
}

// Store enqueues an exchange for async delivery. Non-blocking: if the queue
// is full the exchange is dropped with a warning (we never block the proxy).
func (s *MuninnStore) Store(ex *CapturedExchange) {
	if s.stats != nil {
		s.stats.Captured.Add(1)
		s.stats.RecordModel(ex.Model)
		s.stats.TokensIn.Add(int64(ex.TokensIn))
		s.stats.TokensOut.Add(int64(ex.TokensOut))
		s.stats.CacheWrite.Add(int64(ex.CacheWrite))
		s.stats.CacheRead.Add(int64(ex.CacheRead))
	}

	select {
	case s.queue <- ex:
	default:
		slog.Warn("muninn store queue full, dropping exchange", "path", ex.Path)
		if s.stats != nil {
			s.stats.Dropped.Add(1)
		}
	}
}

// Drain flushes any pending exchanges in the queue and returns. Call this
// during graceful shutdown to avoid losing in-flight captures. The background
// worker exits after drain completes.
func (s *MuninnStore) Drain() {
	close(s.queue)
	<-s.done
}

// HealthCheck pings the MuninnDB MCP health endpoint. Returns nil if
// reachable, or an error describing the failure. Uses a short timeout
// so it doesn't delay startup noticeably.
func (s *MuninnStore) HealthCheck() error {
	healthURL, err := healthURLFromMCP(s.mcpURL)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest("GET", healthURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create health request: %w", err)
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("unreachable at %s: %w", healthURL, err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		slog.Debug("error draining health response body", "err", err)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("unhealthy (HTTP %d) at %s", resp.StatusCode, healthURL)
	}
	return nil
}

// healthURLFromMCP derives the /mcp/health URL from the MCP endpoint URL
// by parsing it properly rather than doing brittle string manipulation.
func healthURLFromMCP(mcpURL string) (string, error) {
	u, err := url.Parse(mcpURL)
	if err != nil {
		return "", fmt.Errorf("invalid MCP URL: %w", err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/health"
	return u.String(), nil
}

// worker runs in a dedicated goroutine, collecting exchanges into batches of
// up to 10 and flushing every 2 seconds. This amortizes MCP call overhead
// while keeping latency bounded. It exits when the queue channel is closed
// (via Drain), flushing any remaining items first.
func (s *MuninnStore) worker() {
	defer close(s.done)

	var batch []*CapturedExchange
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case ex, ok := <-s.queue:
			if !ok {
				if len(batch) > 0 {
					s.flush(batch)
				}
				return
			}
			batch = append(batch, ex)
			if len(batch) >= 10 {
				s.flush(batch)
				batch = nil
			}
		case <-ticker.C:
			if len(batch) > 0 {
				s.flush(batch)
				batch = nil
			}
		}
	}
}

// flush sends a batch to MuninnDB: single items use muninn_remember,
// multiple items use muninn_remember_batch.
func (s *MuninnStore) flush(batch []*CapturedExchange) {
	var err error
	n := int64(len(batch))

	if len(batch) == 1 {
		ex := batch[0]
		concept, content := formatExchange(ex)
		err = s.callTool("muninn_remember", map[string]any{
			"vault":   s.vault,
			"concept": concept,
			"content": content,
			"tags":    buildTags(ex),
			"type":    "observation",
		})
	} else {
		memories := make([]map[string]any, 0, len(batch))
		for _, ex := range batch {
			concept, content := formatExchange(ex)
			memories = append(memories, map[string]any{
				"concept": concept,
				"content": content,
				"tags":    buildTags(ex),
				"type":    "observation",
			})
		}
		err = s.callTool("muninn_remember_batch", map[string]any{
			"vault":    s.vault,
			"memories": memories,
		})
	}

	if s.stats != nil {
		if err != nil {
			s.stats.FlushErrors.Add(n)
		} else {
			s.stats.Flushed.Add(n)
		}
	}
}

// maxRetries is the number of attempts for transient MuninnDB failures.
const maxRetries = 3

// callTool builds a JSON-RPC 2.0 tools/call envelope and sends it to MuninnDB.
// Retries up to maxRetries times with exponential backoff for transient failures
// (network errors, 5xx). Client errors (4xx) are not retried.
func (s *MuninnStore) callTool(name string, args map[string]any) error {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": args,
		},
		"id": time.Now().UnixNano(),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("failed to marshal MCP payload", "err", err)
		return err
	}

	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			backoff := time.Duration(1<<attempt) * time.Second // 2s, 4s
			slog.Debug("retrying MCP call", "attempt", attempt+1, "backoff", backoff, "tool", name)
			time.Sleep(backoff)
		}

		lastErr = s.doRequest(body)
		if lastErr == nil {
			return nil
		}
	}

	slog.Error("MCP call failed after retries", "tool", name, "attempts", maxRetries, "err", lastErr)
	return lastErr
}

// doRequest sends a single MCP request and returns nil on success, or an
// error for transient failures that should be retried.
func (s *MuninnStore) doRequest(body []byte) error {
	req, err := http.NewRequest("POST", s.mcpURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		slog.Debug("error draining MCP response body", "err", err)
	}

	if resp.StatusCode >= 500 {
		return fmt.Errorf("server error: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		// Client error — don't retry, but warn.
		slog.Warn("muninn returned client error", "status", resp.StatusCode)
	}

	return nil
}

// formatExchange builds the concept (short label used as MuninnDB's primary
// search key) and content (full structured body) for one captured exchange.
func formatExchange(ex *CapturedExchange) (concept, content string) {
	concept = fmt.Sprintf("[%s] %s %s -> %d (%dms)",
		ex.Agent, ex.Method, ex.Path, ex.StatusCode, ex.DurationMs)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Agent: %s\n", ex.Agent))
	sb.WriteString(fmt.Sprintf("Time: %s\n", ex.Timestamp.Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("Request: %s %s\n", ex.Method, ex.Path))
	sb.WriteString(fmt.Sprintf("Status: %d | Duration: %dms\n", ex.StatusCode, ex.DurationMs))

	if ex.Model != "" {
		sb.WriteString(fmt.Sprintf("Model: %s\n", ex.Model))
	}
	if ex.TokensIn > 0 || ex.TokensOut > 0 {
		sb.WriteString(fmt.Sprintf("Tokens: %d in / %d out\n", ex.TokensIn, ex.TokensOut))
	}
	if ex.CacheWrite > 0 || ex.CacheRead > 0 {
		sb.WriteString(fmt.Sprintf("Cache: %d write / %d read\n", ex.CacheWrite, ex.CacheRead))
	}

	sb.WriteString("\n--- Request Body ---\n")
	sb.WriteString(truncateJSON(ex.ReqBody, 4096))
	sb.WriteString("\n\n--- Response Body ---\n")
	sb.WriteString(truncateJSON(ex.RespBody, 4096))

	return concept, sb.String()
}

func truncateJSON(data json.RawMessage, maxBytes int) string {
	if len(data) == 0 {
		return "(empty)"
	}
	s := string(data)
	if len(s) > maxBytes {
		return s[:maxBytes] + "\n... (truncated)"
	}
	return s
}

func buildTags(ex *CapturedExchange) []string {
	tags := []string{"sidecar", ex.Agent, fmt.Sprintf("status:%d", ex.StatusCode)}
	if ex.Model != "" {
		tags = append(tags, "model:"+ex.Model)
	}
	return tags
}

// DefaultMCPURL returns the MuninnDB MCP endpoint from MUNINN_MCP_URL,
// falling back to the standard local address.
func DefaultMCPURL() string {
	if u := os.Getenv("MUNINN_MCP_URL"); u != "" {
		return u
	}
	return "http://127.0.0.1:8750/mcp"
}

// DefaultToken reads the MuninnDB bearer token from MUNINN_TOKEN or the
// well-known file at ~/.muninn/mcp.token (the same file MuninnDB writes
// on first start).
func DefaultToken() string {
	if t := os.Getenv("MUNINN_TOKEN"); t != "" {
		return t
	}
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(home + "/.muninn/mcp.token")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
