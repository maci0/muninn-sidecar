// Package store provides async delivery of captured API exchanges to
// MuninnDB via MCP JSON-RPC.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
	"github.com/maci0/muninn-sidecar/internal/mcpclient"
	"github.com/maci0/muninn-sidecar/internal/stats"
)

// dedupRingSize is the number of slots in the dedup ring buffer.
// Each slot holds a set of hashes; the ring advances each flush cycle (~2s).
const dedupRingSize = 8

// formattedMemory holds a pre-formatted exchange ready for MuninnDB.
type formattedMemory struct {
	concept string
	content string
	tags    []string
}

// MuninnStore delivers captured API exchanges to MuninnDB via MCP JSON-RPC.
// Writes are async: Store() enqueues to a buffered channel, and a background
// goroutine batches them (up to 10 per call, flushed every 2s) using
// muninn_remember_batch. This keeps the proxy's hot path free of network I/O.
type MuninnStore struct {
	mcpURL    string             // MCP endpoint, e.g. http://127.0.0.1:8750/mcp
	token     string             // Bearer token for MuninnDB auth; empty = no auth
	vault     string             // target vault in MuninnDB (default: "sidecar")
	mcp       *mcpclient.Client  // shared MCP JSON-RPC client
	queue     chan *CapturedExchange
	done      chan struct{} // closed when Drain completes
	drainOnce sync.Once    // ensures Drain is idempotent
	stats     *stats.Stats // session statistics (nil-safe)
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
	trimmedURL := strings.TrimRight(mcpURL, "/")
	s := &MuninnStore{
		mcpURL: trimmedURL,
		token:  token,
		vault:  vault,
		mcp:    mcpclient.New(trimmedURL, token, 10*time.Second),
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
// worker exits after drain completes. Safe to call multiple times.
func (s *MuninnStore) Drain() {
	s.drainOnce.Do(func() {
		close(s.queue)
	})
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
//
// Each exchange is formatted and deduplicated before batching. The dedup
// ring buffer (8 slots, advanced each flush cycle) prevents duplicate
// concepts from being stored when the same user message generates multiple
// API calls in a tool-use chain. This runs in a single goroutine, so no
// locking is needed for the ring buffer.
func (s *MuninnStore) worker() {
	defer close(s.done)

	var batch []formattedMemory
	var dedupRing [dedupRingSize]map[uint64]struct{}
	ringIdx := 0
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case ex, ok := <-s.queue:
			if !ok {
				if len(batch) > 0 {
					s.flushFormatted(batch)
				}
				return
			}
			if fm := s.formatAndDedup(ex, &dedupRing, &ringIdx); fm != nil {
				batch = append(batch, *fm)
			}
			if len(batch) >= 10 {
				s.flushFormatted(batch)
				batch = nil
			}
		case <-ticker.C:
			if len(batch) > 0 {
				s.flushFormatted(batch)
				batch = nil
			}
			// Advance ring slot each flush cycle to expire old hashes.
			ringIdx = (ringIdx + 1) % dedupRingSize
			dedupRing[ringIdx] = nil
		}
	}
}

// formatAndDedup formats an exchange, strips system-reminders, skips empty
// captures, appends metadata tags, and deduplicates by concept hash. Returns
// nil if the exchange should be dropped. Each ring slot holds a set of hashes
// from one flush cycle, so multiple exchanges per cycle are tracked correctly.
func (s *MuninnStore) formatAndDedup(ex *CapturedExchange, ring *[dedupRingSize]map[uint64]struct{}, ringIdx *int) *formattedMemory {
	userMsg := apiformat.StripSystemReminders(apiformat.ExtractUserMessage(ex.ReqBody))
	assistantMsg := apiformat.ExtractAssistantMessage(ex.RespBody)

	// Build concept.
	var concept string
	if userMsg != "" {
		concept = apiformat.TruncateText(userMsg, 120)
	} else {
		concept = fmt.Sprintf("[%s] %s %s -> %d (%dms)",
			ex.Agent, ex.Method, ex.Path, ex.StatusCode, ex.DurationMs)
	}

	// Build content.
	var sb strings.Builder
	if userMsg != "" {
		sb.WriteString("User:\n")
		sb.WriteString(apiformat.TruncateText(userMsg, 4000))
		sb.WriteString("\n\n")
	}
	if assistantMsg != "" {
		sb.WriteString("Assistant:\n")
		sb.WriteString(apiformat.TruncateText(assistantMsg, 4000))
	}

	// Skip empty captures: if both user and assistant are empty after
	// stripping, this exchange has no meaningful conversation content.
	if userMsg == "" && assistantMsg == "" {
		slog.Debug("skipping empty exchange", "path", ex.Path)
		if s.stats != nil {
			s.stats.Skipped.Add(1)
		}
		return nil
	}

	content := sb.String()

	// Dedup by concept hash (FNV-1a). Skip if seen in any ring slot.
	h := fnv.New64a()
	h.Write([]byte(concept))
	hash := h.Sum64()

	for i := range ring {
		if ring[i] != nil {
			if _, exists := ring[i][hash]; exists {
				slog.Debug("dedup: skipping duplicate concept", "concept", apiformat.TruncateText(concept, 60))
				if s.stats != nil {
					s.stats.Deduped.Add(1)
				}
				return nil
			}
		}
	}
	if ring[*ringIdx] == nil {
		ring[*ringIdx] = make(map[uint64]struct{})
	}
	ring[*ringIdx][hash] = struct{}{}

	return &formattedMemory{
		concept: concept,
		content: content,
		tags:    buildTags(ex),
	}
}

// flushFormatted sends a batch of pre-formatted memories to MuninnDB:
// single items use muninn_remember, multiple use muninn_remember_batch.
func (s *MuninnStore) flushFormatted(batch []formattedMemory) {
	var err error
	n := int64(len(batch))

	if len(batch) == 1 {
		fm := batch[0]
		err = s.callTool("muninn_remember", map[string]any{
			"vault":   s.vault,
			"concept": fm.concept,
			"content": fm.content,
			"tags":    fm.tags,
			"type":    "observation",
		})
	} else {
		memories := make([]map[string]any, 0, len(batch))
		for _, fm := range batch {
			memories = append(memories, map[string]any{
				"concept": fm.concept,
				"content": fm.content,
				"tags":    fm.tags,
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

// maxAttempts is the number of attempts for transient MuninnDB failures.
const maxAttempts = 3

// callTool sends a JSON-RPC 2.0 tools/call request to MuninnDB via the
// shared MCP client. Retries up to maxAttempts with exponential backoff
// for transient failures (network errors, 5xx). Client errors (4xx) are
// not retried.
func (s *MuninnStore) callTool(name string, args map[string]any) error {
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			backoff := time.Duration(1<<attempt) * time.Second // 2s, 4s
			slog.Debug("retrying MCP call", "attempt", attempt+1, "backoff", backoff, "tool", name)
			time.Sleep(backoff)
		}

		_, lastErr = s.mcp.Call(context.Background(), name, args)
		if lastErr == nil {
			return nil
		}
		// Don't retry client errors (4xx) — they won't succeed on retry.
		var ce *mcpclient.ClientError
		if errors.As(lastErr, &ce) {
			return lastErr
		}
	}

	slog.Error("MCP call failed after retries", "tool", name, "attempts", maxAttempts, "err", lastErr)
	return lastErr
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
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".muninn", "mcp.token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
