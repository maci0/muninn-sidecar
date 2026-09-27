// Package store provides async delivery of captured API exchanges to
// MuninnDB via MCP JSON-RPC.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
	"github.com/maci0/muninn-sidecar/internal/clock"
	"github.com/maci0/muninn-sidecar/internal/mcpclient"
	"github.com/maci0/muninn-sidecar/internal/redact"
	"github.com/maci0/muninn-sidecar/internal/reqid"
	"github.com/maci0/muninn-sidecar/internal/stats"
	"github.com/maci0/muninn-sidecar/internal/strhash"
)

// dedupRingSize is the number of slots in the dedup ring buffer.
// Each slot holds a set of hashes; the ring advances each flush cycle (~2s).
const dedupRingSize = 8

// maxBatchSize is the maximum number of memories sent in a single MuninnDB call.
const maxBatchSize = 10

// dropLogEvery throttles the queue-full warning. A full queue drops one
// exchange per Store call, so warning on each drop turns a sustained MuninnDB
// outage into one warn line per agent turn and buries everything else. The
// first drop always warns and every dropLogEvery-th after it; Stats.Dropped
// carries the exact count.
const dropLogEvery = 100

// maxQueuedBytes bounds the body bytes the queue may hold at once. The queue
// depth alone is not a memory bound: an exchange carries a captured request
// that repeats the whole conversation (up to proxy.maxRequestBodySize, 50 MiB)
// plus its response, so a 256-slot queue can legitimately hold tens of GiB and
// the process is OOM-killed long before the slot count says anything is wrong.
// The byte budget is the back-pressure the depth was meant to be: a large
// capture is dropped when the queued bytes are already at the cap, and small
// ones keep flowing. 256 MiB is far above a full queue of ordinary turns and
// far below what a machine running an agent can afford to lose silently.
const maxQueuedBytes = 256 << 20 // 256 MiB

// Rune caps on what one exchange contributes to a memory. A captured request
// carries the whole conversation and can reach tens of MiB, so the concept
// (which the live feed and the dedup key are built from) and the stored content
// are both truncated before they leave the worker.
const (
	// conceptRunesBoth is the user half of a concept that shows both sides of
	// the exchange; conceptRunesSingle is the whole concept when only one side
	// is present. The user side gets more room because it carries the request.
	conceptRunesBoth   = 80
	conceptRunesSingle = 120
	// assistantRunesPreview is the assistant half of a two-sided concept, the
	// short one: the concept only has to be recognizable in the live feed.
	assistantRunesPreview = 40
	// sideRunesBody is each side's share of the stored content.
	sideRunesBody = 4000
)

// queueItem is one exchange in flight through the store, with the body bytes
// it is holding against maxQueuedBytes. The count travels with the exchange so
// the worker releases exactly what the producer reserved, whichever side of the
// handoff fails.
type queueItem struct {
	ex    *CapturedExchange
	bytes int64
}

// formattedMemory holds a pre-formatted exchange ready for MuninnDB.
type formattedMemory struct {
	concept string
	content string
	tags    []string
	// requestID is the correlation ID of the turn this memory came from, kept
	// so a failed flush can name the turns whose memories were not written.
	requestID string
	// hash is this memory's dedup-ring entry. It travels with the memory so the
	// ring can record it only once MuninnDB has actually stored it: marking it
	// at format time would suppress a re-ask of a question whose write failed.
	hash uint64
}

// MuninnStore delivers captured API exchanges to MuninnDB via MCP JSON-RPC.
// Writes are async: Store() enqueues to a buffered channel, and a background
// goroutine batches them (up to 10 per call, flushed every 2s) using
// muninn_remember for single items or muninn_remember_batch for multiple.
// This keeps the proxy's hot path free of network I/O. The queue is bounded
// by both slot count and body bytes (see maxQueuedBytes).
type MuninnStore struct {
	vault     string            // target vault in MuninnDB (default: "sidecar")
	mcp       *mcpclient.Client // shared MCP JSON-RPC client
	queue     chan queueItem    // buffered channel of pending exchanges (depth 256)
	done      chan struct{}     // closed when Drain completes
	drainOnce sync.Once         // ensures Drain is idempotent
	stats     *stats.Stats      // session statistics (nil-safe)
	redact    atomic.Bool       // scrub secrets from captured content before storage

	// queuedBytes is the body bytes reserved against maxQueuedBytes by the
	// producers and released by the worker as each exchange is formatted. The
	// queue depth says how many captures are waiting; this says how much memory
	// they are holding, which is the number that actually runs the process out.
	queuedBytes atomic.Int64

	// queueMu orders a producer's enqueue against Drain's close. A send racing a
	// close is a data race the runtime reports, and it panics the sending
	// goroutine when it loses. drained is guarded by it and set before the close,
	// so an exchange that arrives after the close is dropped and accounted for
	// rather than sent into a channel nobody will read.
	queueMu sync.RWMutex
	drained bool

	// prepare is written by SetPreparer (the caller) and read on the worker
	// goroutine, which is already running by the time it is installed, so it
	// needs its own synchronization rather than the startup ordering.
	prepareMu sync.RWMutex
	prepare   Preparer     // capture-side normalization, run on the worker (nil = bodies stored as captured)
	dropped   atomic.Int64 // exchanges dropped for any reason (drives the throttled warning)

	// flushCtx governs MCP flush calls and their retries. It stays live for the
	// whole session (so transient blips get full retries), and Drain arms a
	// deadline that cancels it — bounding worst-case shutdown when MuninnDB is
	// unreachable instead of retrying 6s per queued batch.
	flushCtx    context.Context
	flushCancel context.CancelFunc

	// clock drives every timing decision the worker makes: the flush ticker,
	// the dedup ring's expiry, the retry backoff, and the drain deadline. It is
	// fixed at construction, so a simulator's scripted clock and the worker's
	// timers cannot race.
	clock clock.Clock
}

// drainTimeout bounds total shutdown flushing. It exceeds one batch's full retry
// budget (2s+4s backoff) so a single in-flight batch can still recover from a
// transient blip, while a large backlog against an unreachable MuninnDB is
// bounded to one budget instead of ~6s per queued batch (which could be minutes).
const drainTimeout = 8 * time.Second

// Preparer normalizes a captured exchange in place before it is formatted,
// redacted, and stored: it strips the bodies of content that must never reach
// memory (injected context, MuninnDB's own tool traffic) and fills in the
// derived fields (Model, token counts) that the exchange is not born with.
// It runs on the worker's goroutine, never on the caller's request path.
type Preparer func(*CapturedExchange)

// CapturedExchange holds one request->response pair captured by the proxy.
// The bodies arrive as the agent sent and the upstream replied; the Model and
// token-count fields are derived from them and filled in by the Preparer
// (nil Preparer leaves them at zero).
type CapturedExchange struct {
	Agent string `json:"agent"` // which coding agent (claude, codex, etc.)
	// RequestID is the correlation ID minted at ingress (see internal/reqid).
	// The worker logs about exchanges long after the request that produced them
	// returned, interleaved with other turns' exchanges, so without this a
	// store warning cannot be traced back to the turn that caused it.
	RequestID  string          `json:"-"`
	Path       string          `json:"path"` // request path (e.g. /v1/messages)
	ReqBody    json.RawMessage `json:"req_body,omitempty"`
	StatusCode int             `json:"status_code"`
	RespBody   json.RawMessage `json:"resp_body,omitempty"`
	Model      string          `json:"model,omitempty"`       // extracted from req/resp JSON
	TokensIn   int             `json:"tokens_in,omitempty"`   // input/prompt token count
	TokensOut  int             `json:"tokens_out,omitempty"`  // output/completion token count
	CacheWrite int             `json:"cache_write,omitempty"` // Anthropic cache_creation_input_tokens
	CacheRead  int             `json:"cache_read,omitempty"`  // Anthropic cache_read_input_tokens
	DurationMs int64           `json:"duration_ms,omitempty"` // request arrival to last response byte, in ms

	// UserText and AssistantText hold the messages a Preparer already pulled
	// out of ReqBody and RespBody while normalizing them, before the store's own
	// normalization (system-reminder stripping, redaction). A captured request
	// carries the whole conversation and can reach tens of MiB, so decoding it
	// a second time here to find the same two strings is the store worker's
	// dominant cost. A nil field means "not extracted yet" and the store falls
	// back to decoding the body itself.
	UserText      *string `json:"-"`
	AssistantText *string `json:"-"`
}

// New creates a MuninnStore and starts its background flush goroutine.
// The queue depth of 256 and the maxQueuedBytes budget together provide
// back-pressure: if MuninnDB is unreachable for an extended period, new
// captures are dropped rather than letting memory grow unbounded. Pass a
// non-nil Stats to track session metrics.
func New(mcpURL, token, vault string, st *stats.Stats) *MuninnStore {
	return NewWithClock(mcpURL, token, vault, st, nil)
}

// NewWithClock is New with an explicit clock; a nil clock means the system
// clock. Every timing decision the worker makes comes from it, so a simulator
// can drive flushes, dedup expiry, and retry backoff from a script and get the
// same delivery sequence on every run.
func NewWithClock(mcpURL, token, vault string, st *stats.Stats, clk clock.Clock) *MuninnStore {
	if vault == "" {
		vault = "sidecar"
	}
	if clk == nil {
		clk = clock.SystemClock{}
	}
	s := &MuninnStore{
		vault: vault,
		// 10-second timeout: store ops run on a background goroutine and are
		// not latency-sensitive, so a generous timeout allows for transient
		// slowness without losing captures prematurely.
		mcp:   mcpclient.New(mcpURL, token, 10*time.Second),
		queue: make(chan queueItem, 256),
		done:  make(chan struct{}),
		stats: st,
		clock: clk,
	}
	s.flushCtx, s.flushCancel = context.WithCancel(context.Background())
	s.redact.Store(true) // secure default: scrub secrets before storage
	go s.worker()
	return s
}

// SetRedaction enables or disables secret redaction of captured content before
// storage. Redaction is on by default; disable it (e.g. via --no-redact) only in
// trusted environments where full-fidelity capture is wanted. Call before
// captures start flowing.
func (s *MuninnStore) SetRedaction(enabled bool) { s.redact.Store(enabled) }

// SetPreparer installs the capture-side normalization applied to each exchange
// before it is formatted (see Preparer). The proxy installs one so the bodies it
// hands over are raw: stripping injected context and muninn tool traffic, and
// extracting model/usage, all parse bodies that reach tens of MiB, and that
// work belongs on the worker's goroutine rather than in the agent's turn.
// Safe to call at any time; exchanges already queued are normalized by the
// preparer in force when the worker reaches them.
func (s *MuninnStore) SetPreparer(p Preparer) {
	s.prepareMu.Lock()
	s.prepare = p
	s.prepareMu.Unlock()
}

// preparer returns the installed Preparer, or nil when none is set.
func (s *MuninnStore) preparer() Preparer {
	s.prepareMu.RLock()
	defer s.prepareMu.RUnlock()
	return s.prepare
}

// Store enqueues an exchange for async delivery. Non-blocking: if the queue is
// full, or the bytes already queued are at maxQueuedBytes, the exchange is
// dropped with a warning (we never block the proxy). Safe to call after
// Drain() — late arrivals are dropped with a warning.
//
// The exchange is enqueued as captured: normalization (Preparer), redaction,
// formatting, and the model/token counters all run later, on the worker's
// goroutine, so this call does no JSON work on the agent's request path. An
// exchange dropped here is therefore never counted toward token usage.
func (s *MuninnStore) Store(ex *CapturedExchange) {
	if s.stats != nil {
		s.stats.Captured.Add(1)
	}

	n := int64(len(ex.ReqBody)) + int64(len(ex.RespBody))
	if !s.reserve(n) {
		// Nothing was reserved, so there is nothing to give back: count the
		// drop only. Calling drop here would return bytes this call never took
		// and drive the budget negative.
		s.warnDrop(ex, "muninn store byte budget exhausted, dropping exchanges",
			"bytes", n, "limit", maxQueuedBytes)
		return
	}

	// The enqueue and Drain's close have to be ordered. A send that races a
	// close is a data race, and losing the race panics this goroutine inside a
	// deferred recover that also has to unwind the byte reservation — the
	// accounting of a dropped memory hanging off a panic. The read lock spans
	// only the non-blocking send; Drain takes the write lock to set drained and
	// close, so every exchange either reached the queue (a closed channel still
	// hands the worker its buffered items) or is dropped here.
	s.queueMu.RLock()
	if s.drained {
		s.queueMu.RUnlock()
		s.drop(ex, n, "muninn store: dropped exchange after drain")
		return
	}
	select {
	case s.queue <- queueItem{ex: ex, bytes: n}:
		s.queueMu.RUnlock()
	default:
		s.queueMu.RUnlock()
		s.drop(ex, n, "muninn store queue full, dropping exchanges")
	}
}

// drop accounts for an exchange that will not be delivered and returns the
// bytes it reserved to the budget.
func (s *MuninnStore) drop(ex *CapturedExchange, n int64, reason string, extra ...any) {
	s.release(n)
	s.warnDrop(ex, reason, extra...)
}

// warnDrop counts one dropped exchange and logs it, throttled so a sustained
// outage does not bury every other line. reason is the warning's message;
// extra are additional key/value pairs for it.
func (s *MuninnStore) warnDrop(ex *CapturedExchange, reason string, extra ...any) {
	if s.stats != nil {
		s.stats.Dropped.Add(1)
	}
	c := s.dropped.Add(1)
	if c != 1 && c%dropLogEvery != 0 {
		return
	}
	slog.Warn(reason, append([]any{reqid.Field, ex.RequestID, "path", ex.Path, "dropped_total", c}, extra...)...)
}

// reserve claims n bytes of the queue's memory budget, reporting false when
// that would take the queued bytes past maxQueuedBytes. The comparison and the
// claim are one CAS, so concurrent producers cannot both see headroom the other
// just spent.
func (s *MuninnStore) reserve(n int64) bool {
	for {
		cur := s.queuedBytes.Load()
		if cur+n > maxQueuedBytes {
			return false
		}
		if s.queuedBytes.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}

// release returns n bytes to the queue's memory budget once the exchange that
// reserved them has been formatted and its bodies are no longer referenced.
func (s *MuninnStore) release(n int64) { s.queuedBytes.Add(-n) }

// Drain signals the background worker to stop accepting new exchanges, then
// blocks until all pending exchanges are flushed to MuninnDB and the worker
// exits. Call this during graceful shutdown to avoid losing in-flight captures.
// Safe to call multiple times.
func (s *MuninnStore) Drain() {
	s.drainOnce.Do(func() {
		// Bound shutdown: cancel flush retries after drainTimeout so a queued
		// backlog against an unreachable MuninnDB can't hang exit. Normal flushes
		// before this fires keep their full retry budget. A drain that finishes
		// early stops the timer, so a fast shutdown does not leave a pending
		// callback holding the store for the full drainTimeout.
		timer := s.clock.AfterFunc(drainTimeout, s.flushCancel)
		// Take the write lock so no producer is mid-send when the channel closes:
		// the close either follows every accepted enqueue or precedes every later
		// one, never interleaves with one.
		s.queueMu.Lock()
		s.drained = true
		close(s.queue)
		s.queueMu.Unlock()
		<-s.done
		timer.Stop()
	})
	<-s.done
	s.flushCancel() // release the context once the worker has exited
}

// QueueDepth reports how many captured exchanges are waiting for the worker
// and the queue's capacity. The queue is the back-pressure the store applies
// when MuninnDB is slow or unreachable: at capacity, Store drops the incoming
// exchange, so a depth pinned at the cap is the leading indicator that
// memories are being lost.
func (s *MuninnStore) QueueDepth() (depth, capacity int) {
	return len(s.queue), cap(s.queue)
}

// QueueBytes reports the body bytes currently reserved against the queue's
// memory budget and that budget. A saturated byte budget drops captures the
// same way a full queue does, but at a depth well under the cap, which is the
// signature of a handful of very large exchanges rather than a MuninnDB
// backlog. The status endpoint needs both numbers to tell those apart.
func (s *MuninnStore) QueueBytes() (inFlight, capacity int64) {
	return s.queuedBytes.Load(), maxQueuedBytes
}

// HealthCheck pings the MuninnDB MCP health endpoint for this store's
// configured endpoint. Delegates to mcpclient.Client.HealthCheck.
func (s *MuninnStore) HealthCheck() error {
	return s.mcp.HealthCheck()
}

// Close releases the store's MCP connection pool. Call it after Drain, once the
// worker has flushed everything, so the idle connections to MuninnDB and their
// goroutines do not outlive the session that opened them.
func (s *MuninnStore) Close() { s.mcp.Close() }

// worker runs in a dedicated goroutine, collecting exchanges into batches of
// up to 10 and flushing every 2 seconds. This amortizes MCP call overhead
// while keeping latency bounded. It exits when the queue channel is closed
// (via Drain), flushing any remaining items first.
//
// The worker is the only place capture-side normalization runs: each exchange
// passes through the Preparer, then formatting, redaction, and dedup, and the
// per-model and token counters are recorded from the prepared exchange. The
// dedup ring buffer (8 slots, advanced each flush cycle) prevents duplicate
// concepts from being stored when the same user message generates multiple
// API calls in a tool-use chain. All of this runs in a single goroutine, so no
// locking is needed for the ring buffer.
func (s *MuninnStore) worker() {
	defer close(s.done)

	var batch []formattedMemory
	// pending holds the hashes of the memories in the batch that has not been
	// delivered yet, so a repeat within one batch still collapses. It is
	// consulted alongside the ring while formatting and is merged into the ring
	// only once the flush succeeds.
	pending := make(map[uint64]struct{})
	var dedupRing [dedupRingSize]map[uint64]struct{}
	ringIdx := 0
	ticker := s.clock.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case item, ok := <-s.queue:
			if !ok {
				if len(batch) > 0 {
					s.flushDelivered(batch, &dedupRing, &ringIdx)
				}
				return
			}
			fm := s.prepareForStore(item.ex, &dedupRing, pending, &ringIdx)
			// The exchange is prepared: the batch keeps only the truncated
			// concept and content, so the reserved bodies are unreachable and
			// their budget returns to the pool.
			s.release(item.bytes)
			if fm != nil {
				pending[fm.hash] = struct{}{}
				batch = append(batch, *fm)
			}
			if len(batch) >= maxBatchSize {
				s.flushDelivered(batch, &dedupRing, &ringIdx)
				batch, pending = nil, make(map[uint64]struct{})
			}
		case <-ticker.C():
			// Roll the dedup window before flushing. The batch about to be
			// delivered is recorded into the slot this tick opens, so it lives
			// the full dedupRingSize cycles the window promises; rolling after
			// the flush would clear the very slot that flush had just written,
			// leaving a one-cycle (~2s) window instead of the ~16s one.
			ringIdx = (ringIdx + 1) % dedupRingSize
			dedupRing[ringIdx] = nil
			if len(batch) > 0 {
				s.flushDelivered(batch, &dedupRing, &ringIdx)
				batch, pending = nil, make(map[uint64]struct{})
			}
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

// prepareForStore runs the capture-side Preparer (when installed), records the
// model and token usage it derived, then formats and deduplicates the
// exchange. Returns nil if the exchange should be dropped.
func (s *MuninnStore) prepareForStore(ex *CapturedExchange, ring *[dedupRingSize]map[uint64]struct{}, pending map[uint64]struct{}, ringIdx *int) *formattedMemory {
	if p := s.preparer(); p != nil {
		p(ex)
	}
	if s.stats != nil {
		s.stats.RecordModel(ex.Model)
		s.stats.TokensIn.Add(int64(ex.TokensIn))
		s.stats.TokensOut.Add(int64(ex.TokensOut))
		s.stats.CacheWrite.Add(int64(ex.CacheWrite))
		s.stats.CacheRead.Add(int64(ex.CacheRead))
	}
	return s.formatAndDedup(ex, ring, pending, ringIdx)
}

// formatAndDedup formats an exchange, strips system-reminders, skips empty
// captures, filters noise content, appends metadata tags, and deduplicates
// by concept hash. Returns nil if the exchange should be dropped. Duplicates
// are looked up in pending (the undelivered batch) and in the ring (memories
// already stored), and the hash is returned with the memory rather than
// recorded here: it enters the ring only once the flush that writes it
// succeeds, so a failed write never leaves a mark that suppresses the retry.
func (s *MuninnStore) formatAndDedup(ex *CapturedExchange, ring *[dedupRingSize]map[uint64]struct{}, pending map[uint64]struct{}, ringIdx *int) *formattedMemory {
	userMsg := apiformat.StripSystemReminders(exchangeText(ex.UserText, func() string {
		return apiformat.ExtractUserMessage(ex.ReqBody)
	}))
	assistantMsg := exchangeText(ex.AssistantText, func() string {
		return apiformat.ExtractAssistantMessage(ex.RespBody)
	})

	// Redact well-known secret formats (API keys, tokens, private keys) before
	// they enter long-term memory, where they would persist and resurface on
	// recall. Applied here so both the concept and content are scrubbed.
	if s.redact.Load() {
		userMsg = redact.Secrets(userMsg)
		assistantMsg = redact.Secrets(assistantMsg)
	}

	// Skip system-generated noise (context continuations, summary tasks).
	if isNoiseContent(userMsg) {
		slog.Debug("skipping noise content", reqid.Field, ex.RequestID, "path", ex.Path)
		if s.stats != nil {
			s.stats.Skipped.Add(1)
		}
		return nil
	}

	// Skip empty captures: if both user and assistant are empty after
	// stripping, this exchange has no meaningful conversation content.
	if userMsg == "" && assistantMsg == "" {
		slog.Debug("skipping empty exchange", reqid.Field, ex.RequestID, "path", ex.Path)
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
		concept = apiformat.TruncateText(userMsg, conceptRunesBoth) +
			" → " + apiformat.TruncateText(assistantMsg, assistantRunesPreview)
	case userMsg != "":
		concept = apiformat.TruncateText(userMsg, conceptRunesSingle)
	default:
		concept = apiformat.TruncateText(assistantMsg, conceptRunesSingle)
	}

	var sb strings.Builder
	if userMsg != "" {
		sb.WriteString("User:\n")
		sb.WriteString(apiformat.TruncateText(userMsg, sideRunesBody))
		sb.WriteString("\n\n")
	}
	if assistantMsg != "" {
		sb.WriteString("Assistant:\n")
		sb.WriteString(apiformat.TruncateText(assistantMsg, sideRunesBody))
	}

	content := sb.String()

	// Dedup by concept hash (FNV-1a). Skip if seen in any ring slot.
	hash := strhash.FNV1a(concept)

	for i := range ring {
		if ring[i] != nil {
			if _, exists := ring[i][hash]; exists {
				// Log the dedup hash, not the concept text: the concept is derived
				// from captured conversation content and would leak into logs
				// (bypassing the redaction applied above) if emitted verbatim.
				slog.Debug("dedup: skipping duplicate concept", reqid.Field, ex.RequestID, "hash", hash)
				if s.stats != nil {
					s.stats.Deduped.Add(1)
				}
				return nil
			}
		}
	}
	if _, exists := pending[hash]; exists {
		slog.Debug("dedup: skipping duplicate concept already in this batch", reqid.Field, ex.RequestID, "hash", hash)
		if s.stats != nil {
			s.stats.Deduped.Add(1)
		}
		return nil
	}

	return &formattedMemory{
		concept:   concept,
		content:   content,
		tags:      buildTags(ex),
		requestID: ex.RequestID,
		hash:      hash,
	}
}

// exchangeText returns the message text a Preparer already extracted, or
// extracts it from the body when no Preparer supplied it. The fallback is a
// closure so the body is only decoded when it is actually needed.
func exchangeText(prepared *string, extract func() string) string {
	if prepared != nil {
		return *prepared
	}
	return extract()
}

// flushDelivered sends a batch and records its dedup hashes in the ring only if
// the write landed. The ring is the store's ledger of "this concept is already
// in the vault", so a hash entered for a batch that failed would suppress a
// re-ask of the same question for the rest of the window and lose a memory
// that was never stored. A failed batch is dropped and its hashes are left
// unrecorded, so the next occurrence of the concept is formatted and sent
// again.
func (s *MuninnStore) flushDelivered(batch []formattedMemory, ring *[dedupRingSize]map[uint64]struct{}, ringIdx *int) {
	if s.flushFormatted(batch) {
		if ring[*ringIdx] == nil {
			ring[*ringIdx] = make(map[uint64]struct{})
		}
		for _, fm := range batch {
			ring[*ringIdx][fm.hash] = struct{}{}
		}
	}
}

// flushFormatted sends a batch of pre-formatted memories to MuninnDB:
// single items use muninn_remember, multiple use muninn_remember_batch.
// Every memory carries a content-addressed dedup_key (mcpclient.DedupKey), and
// the whole call reuses one JSON-RPC request id across attempts (see callTool),
// so a retry of an ambiguous write is recognisable as the same operation on
// both sides. Reports whether the batch is stored.
func (s *MuninnStore) flushFormatted(batch []formattedMemory) bool {
	var err error
	n := int64(len(batch))

	if len(batch) == 1 {
		fm := batch[0]
		err = s.callTool("muninn_remember", map[string]any{
			"vault":     s.vault,
			"concept":   fm.concept,
			"content":   fm.content,
			"tags":      fm.tags,
			"type":      "observation",
			"dedup_key": mcpclient.DedupKey(s.vault, fm.concept, fm.content),
		})
	} else {
		memories := make([]map[string]any, 0, len(batch))
		for _, fm := range batch {
			memories = append(memories, map[string]any{
				"concept":   fm.concept,
				"content":   fm.content,
				"tags":      fm.tags,
				"type":      "observation",
				"dedup_key": mcpclient.DedupKey(s.vault, fm.concept, fm.content),
			})
		}
		err = s.callTool("muninn_remember_batch", map[string]any{
			"vault":    s.vault,
			"memories": memories,
		})
	}

	if err != nil {
		// Deliberately omit concept/content here: this is a default-visible
		// Error log and the concept is captured conversation text. Redaction
		// scrubs secrets/emails but not every form of personal data, so keep
		// it out of logs entirely. vault, batch_size, and the turns' correlation
		// IDs are enough to triage and to find the request logs for the lost
		// memories.
		slog.Error("failed to flush exchanges to MuninnDB",
			"vault", s.vault, "batch_size", n, reqid.Field, batchRequestIDs(batch), "err", err)
		if s.stats != nil {
			s.stats.FlushErrors.Add(n)
		}
		return false
	}
	slog.Debug("flushed exchanges to MuninnDB", "vault", s.vault, "batch_size", n)
	if s.stats != nil {
		s.stats.Flushed.Add(n)
	}
	return true
}

// maxFlushLogIDs caps the correlation IDs a failed-flush line names. A batch
// holds up to maxBatchSize exchanges; naming all of them would bury the error
// under a list, and the drop count plus the session's captured/latency numbers
// say how much was lost without it.
const maxFlushLogIDs = 3

// batchRequestIDs renders the correlation IDs of the turns in a failed batch,
// comma-separated and capped at maxFlushLogIDs. Returns "unknown" for a batch
// of exchanges that carry no ID, so the field is never silently empty.
func batchRequestIDs(batch []formattedMemory) string {
	ids := make([]string, 0, min(len(batch), maxFlushLogIDs))
	for _, fm := range batch {
		if len(ids) == maxFlushLogIDs {
			ids = append(ids, "...")
			break
		}
		id := fm.requestID
		if id == "" {
			id = "unknown"
		}
		ids = append(ids, id)
	}
	return strings.Join(ids, ",")
}

// maxAttempts is the number of attempts for transient MuninnDB failures.
const maxAttempts = 3

// callTool sends a JSON-RPC 2.0 tools/call request to MuninnDB via the
// shared MCP client. Retries up to maxAttempts with exponential backoff
// for transient failures (network errors, 5xx). Client errors (4xx) and
// RPC protocol errors are not retried.
//
// The request ID is reserved once, before the first attempt, and reused by
// every retry: a fresh per-attempt ID would make each retry look like a new
// operation, so a first attempt that committed before its response was lost
// would be written a second time. The remember tools carry a per-memory
// dedup_key for the same reason.
func (s *MuninnStore) callTool(name string, args map[string]any) error {
	id := mcpclient.NextRequestID()
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			backoff := time.Duration(1<<attempt) * time.Second // 2s, 4s
			slog.Debug("retrying MCP call", "attempt", attempt+1, "backoff", backoff, "tool", name, "err", lastErr)
			// Interruptible backoff: a Drain deadline cancels flushCtx so we
			// don't sleep through shutdown.
			select {
			case <-s.clock.After(backoff):
			case <-s.flushCtx.Done():
				return s.flushCtx.Err()
			}
		}

		body, err := s.mcp.CallWithID(s.flushCtx, id, name, args)
		lastErr = err
		if lastErr == nil {
			// A 2xx that is not a JSON-RPC response object confirms nothing:
			// an intermediary's HTML page or a truncated body would be
			// reported as a flushed batch with nothing stored.
			if envErr := mcpclient.CheckEnvelope(body); envErr != nil {
				lastErr = fmt.Errorf("%s: %w", name, envErr)
			} else {
				return nil
			}
		}
		// Stop retrying once the flush context is cancelled (shutdown deadline).
		if s.flushCtx.Err() != nil {
			return lastErr
		}
		// Don't retry client errors (4xx) or RPC-level errors — they
		// indicate a permanent rejection that won't succeed on retry.
		if !retryable(lastErr) {
			return lastErr
		}
	}

	return lastErr
}

// retryable reports whether another attempt at the same logical write could
// plausibly succeed. 4xx responses and JSON-RPC error objects are permanent
// rejections; everything else (transport failures, 5xx) is treated as
// transient. Errors wrapped in context still classify through errors.As, so
// the decision survives a caller adding context on the way up.
func retryable(err error) bool {
	var ce *mcpclient.ClientError
	if errors.As(err, &ce) {
		return false
	}
	var re *mcpclient.RPCError
	return !errors.As(err, &re)
}

func buildTags(ex *CapturedExchange) []string {
	tags := []string{"sidecar", ex.Agent, "status:" + strconv.Itoa(ex.StatusCode)}
	if ex.Model != "" {
		tags = append(tags, "model:"+ex.Model)
	}
	return tags
}
