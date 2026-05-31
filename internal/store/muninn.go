// Package store provides async delivery of captured API exchanges to
// MuninnDB via MCP JSON-RPC.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"log/slog"
	"strconv"
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

// maxBatchSize is the maximum number of memories sent in a single MuninnDB call.
const maxBatchSize = 10

// formattedMemory holds a pre-formatted exchange ready for MuninnDB.
type formattedMemory struct {
	concept string
	content string
	tags    []string
}

// MuninnStore delivers captured API exchanges to MuninnDB via MCP JSON-RPC.
// Writes are async: Store() enqueues to a buffered channel, and a background
// goroutine batches them (up to 10 per call, flushed every 2s) using
// muninn_remember for single items or muninn_remember_batch for multiple.
// This keeps the proxy's hot path free of network I/O.
type MuninnStore struct {
	vault     string                 // target vault in MuninnDB (default: "sidecar")
	mcp       *mcpclient.Client      // shared MCP JSON-RPC client
	queue     chan *CapturedExchange // buffered channel of pending exchanges (depth 256)
	done      chan struct{}          // closed when Drain completes
	drainOnce sync.Once              // ensures Drain is idempotent
	stats     *stats.Stats           // session statistics (nil-safe)

	// flushCtx governs MCP flush calls and their retries. It stays live for the
	// whole session (so transient blips get full retries), and Drain arms a
	// deadline that cancels it — bounding worst-case shutdown when MuninnDB is
	// unreachable instead of retrying 6s per queued batch.
	flushCtx    context.Context
	flushCancel context.CancelFunc
}

// drainTimeout bounds total shutdown flushing. It exceeds one batch's full retry
// budget (2s+4s backoff) so a single in-flight batch can still recover from a
// transient blip, while a large backlog against an unreachable MuninnDB is
// bounded to one budget instead of ~6s per queued batch (which could be minutes).
const drainTimeout = 8 * time.Second

// CapturedExchange holds one request->response pair captured by the proxy.
type CapturedExchange struct {
	Timestamp  time.Time       `json:"timestamp"`
	Agent      string          `json:"agent"`  // which coding agent (claude, gemini, etc.)
	Method     string          `json:"method"` // HTTP method
	Path       string          `json:"path"`   // request path (e.g. /v1/messages)
	ReqBody    json.RawMessage `json:"req_body,omitempty"`
	StatusCode int             `json:"status_code"`
	RespBody   json.RawMessage `json:"resp_body,omitempty"`
	DurationMs int64           `json:"duration_ms"`
	Model      string          `json:"model,omitempty"`       // extracted from req/resp JSON
	TokensIn   int             `json:"tokens_in,omitempty"`   // input/prompt token count
	TokensOut  int             `json:"tokens_out,omitempty"`  // output/completion token count
	CacheWrite int             `json:"cache_write,omitempty"` // Anthropic cache_creation_input_tokens
	CacheRead  int             `json:"cache_read,omitempty"`  // Anthropic cache_read_input_tokens
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
		vault: vault,
		// 10-second timeout: store ops run on a background goroutine and are
		// not latency-sensitive, so a generous timeout allows for transient
		// slowness without losing captures prematurely.
		mcp:   mcpclient.New(mcpURL, token, 10*time.Second),
		queue: make(chan *CapturedExchange, 256),
		done:  make(chan struct{}),
		stats: st,
	}
	s.flushCtx, s.flushCancel = context.WithCancel(context.Background())
	go s.worker()
	return s
}

// Store enqueues an exchange for async delivery. Non-blocking: if the queue
// is full the exchange is dropped with a warning (we never block the proxy).
// Safe to call after Drain() — late arrivals are dropped with a warning.
func (s *MuninnStore) Store(ex *CapturedExchange) {
	if s.stats != nil {
		s.stats.Captured.Add(1)
		s.stats.RecordModel(ex.Model)
		s.stats.TokensIn.Add(int64(ex.TokensIn))
		s.stats.TokensOut.Add(int64(ex.TokensOut))
		s.stats.CacheWrite.Add(int64(ex.CacheWrite))
		s.stats.CacheRead.Add(int64(ex.CacheRead))
	}

	// Recover from panic if queue has been closed by Drain(). This is
	// cheaper than adding a mutex on every Store() call and only fires
	// in the narrow window between Drain() and full shutdown.
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("muninn store: dropped exchange after drain", "path", ex.Path)
			if s.stats != nil {
				s.stats.Dropped.Add(1)
			}
		}
	}()

	select {
	case s.queue <- ex:
	default:
		slog.Warn("muninn store queue full, dropping exchange", "path", ex.Path)
		if s.stats != nil {
			s.stats.Dropped.Add(1)
		}
	}
}

// Drain signals the background worker to stop accepting new exchanges, then
// blocks until all pending exchanges are flushed to MuninnDB and the worker
// exits. Call this during graceful shutdown to avoid losing in-flight captures.
// Safe to call multiple times.
func (s *MuninnStore) Drain() {
	s.drainOnce.Do(func() {
		// Bound shutdown: cancel flush retries after drainTimeout so a queued
		// backlog against an unreachable MuninnDB can't hang exit. Normal flushes
		// before this fires keep their full retry budget.
		time.AfterFunc(drainTimeout, s.flushCancel)
		close(s.queue)
	})
	<-s.done
	s.flushCancel() // release the context once the worker has exited
}

// HealthCheck pings the MuninnDB MCP health endpoint for this store's
// configured endpoint. Delegates to mcpclient.Client.HealthCheck.
func (s *MuninnStore) HealthCheck() error {
	return s.mcp.HealthCheck()
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
			if len(batch) >= maxBatchSize {
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

// noisePatterns are prefixes of user messages that indicate system-generated
// content from coding agent internals rather than meaningful user conversation.
// These pollute the memory store with large, repetitive content that has no
// value for future recall.
var noisePatterns = []string{
	// Claude Code context continuation when conversation runs out of context.
	"This session is being continued from a previous conversation",
	// Claude Code internal summarization mechanism.
	"Your task is to create a detailed summary",
}

// isNoiseContent returns true if a user message matches a known noise pattern
// that should not be stored as a memory.
func isNoiseContent(msg string) bool {
	for _, prefix := range noisePatterns {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}

// formatAndDedup formats an exchange, strips system-reminders, skips empty
// captures, filters noise content, appends metadata tags, and deduplicates
// by concept hash. Returns nil if the exchange should be dropped. Each ring
// slot holds a set of hashes from one flush cycle, so multiple exchanges per
// cycle are tracked correctly.
func (s *MuninnStore) formatAndDedup(ex *CapturedExchange, ring *[dedupRingSize]map[uint64]struct{}, ringIdx *int) *formattedMemory {
	userMsg := apiformat.StripSystemReminders(apiformat.ExtractUserMessage(ex.ReqBody))
	assistantMsg := apiformat.ExtractAssistantMessage(ex.RespBody)

	// Redact well-known secret formats (API keys, tokens, private keys) before
	// they enter long-term memory, where they would persist and resurface on
	// recall. Applied here so both the concept and content are scrubbed.
	userMsg = redactSecrets(userMsg)
	assistantMsg = redactSecrets(assistantMsg)

	// Skip system-generated noise (context continuations, summary tasks).
	if isNoiseContent(userMsg) {
		slog.Debug("skipping noise content", "path", ex.Path)
		if s.stats != nil {
			s.stats.Skipped.Add(1)
		}
		return nil
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

	// Build concept: include both user query and assistant preview so
	// the live feed shows both sides of the conversation, not just the
	// user's request.
	var concept string
	switch {
	case userMsg != "" && assistantMsg != "":
		concept = apiformat.TruncateText(userMsg, 80) + " → " + apiformat.TruncateText(assistantMsg, 40)
	case userMsg != "":
		concept = apiformat.TruncateText(userMsg, 120)
	default:
		concept = apiformat.TruncateText(assistantMsg, 120)
	}

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

	if err != nil {
		slog.Error("failed to flush exchanges to MuninnDB", "vault", s.vault, "batch_size", n, "concept", apiformat.TruncateText(batch[0].concept, 60), "err", err)
		if s.stats != nil {
			s.stats.FlushErrors.Add(n)
		}
	} else {
		slog.Debug("flushed exchanges to MuninnDB", "vault", s.vault, "batch_size", n)
		if s.stats != nil {
			s.stats.Flushed.Add(n)
		}
	}
}

// maxAttempts is the number of attempts for transient MuninnDB failures.
const maxAttempts = 3

// callTool sends a JSON-RPC 2.0 tools/call request to MuninnDB via the
// shared MCP client. Retries up to maxAttempts with exponential backoff
// for transient failures (network errors, 5xx). Client errors (4xx) and
// RPC protocol errors are not retried.
func (s *MuninnStore) callTool(name string, args map[string]any) error {
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			backoff := time.Duration(1<<attempt) * time.Second // 2s, 4s
			slog.Debug("retrying MCP call", "attempt", attempt+1, "backoff", backoff, "tool", name, "err", lastErr)
			// Interruptible backoff: a Drain deadline cancels flushCtx so we
			// don't sleep through shutdown.
			select {
			case <-time.After(backoff):
			case <-s.flushCtx.Done():
				return s.flushCtx.Err()
			}
		}

		_, lastErr = s.mcp.Call(s.flushCtx, name, args)
		if lastErr == nil {
			return nil
		}
		// Stop retrying once the flush context is cancelled (shutdown deadline).
		if s.flushCtx.Err() != nil {
			return lastErr
		}
		// Don't retry client errors (4xx) or RPC-level errors — they
		// indicate a permanent rejection that won't succeed on retry.
		var ce *mcpclient.ClientError
		if errors.As(lastErr, &ce) {
			return lastErr
		}
		var re *mcpclient.RPCError
		if errors.As(lastErr, &re) {
			return lastErr
		}
	}

	return lastErr
}

func buildTags(ex *CapturedExchange) []string {
	tags := []string{"sidecar", ex.Agent, "status:" + strconv.Itoa(ex.StatusCode)}
	if ex.Model != "" {
		tags = append(tags, "model:"+ex.Model)
	}
	return tags
}
