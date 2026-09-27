// Package inject provides automatic memory retrieval and injection into
// LLM API requests. It recalls relevant memories from MuninnDB and injects
// them as system-level context before forwarding requests upstream.
package inject

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
	"github.com/maci0/muninn-sidecar/internal/grounding"
	"github.com/maci0/muninn-sidecar/internal/mcpclient"
	"github.com/maci0/muninn-sidecar/internal/redact"
	"github.com/maci0/muninn-sidecar/internal/stats"
)

// Config holds parameters for creating an Injector.
type Config struct {
	MCPURL        string        // MuninnDB MCP endpoint
	Token         string        // Bearer token for auth
	Vault         string        // vault to recall from (default: "sidecar")
	Budget        int           // max approximate tokens to inject (default: 2048)
	Threshold     float64       // recall floor sent to MuninnDB, on its *composite* score (default: 0.05). Must stay below the gate's calibration floor (calibMinThreshold) so this server-side pre-filter never drops a memory the client-side cosine gate would accept — see New().
	MinScore      float64       // injection threshold: a memory is injected only if its effective score >= MinScore; a turn where nothing clears it injects nothing (default: 0.6)
	RecallMode    string        // MuninnDB recall mode: semantic|recent|balanced|deep (default: "semantic")
	QuerySimReuse float64       // reuse window (skip recall) when query word-set Jaccard vs last query >= this; 1.0 = exact-match only (default)
	AutoCalibrate bool          // self-tune MinScore from observed recall-score distribution (the sidecar enables this; New() callers default off)
	Timeout       time.Duration // MCP call timeout (default: 200ms)
	Stats         *stats.Stats  // session statistics (nil-safe)

	// Grounder, when set, adds an LLM answer-grounding rerank after the cosine
	// gate: each freshly-recalled candidate (top GroundTopK by score) is dropped
	// unless the model judges it actually answers the query. This is the
	// cross-encoder precision step the cosine gate can't do — it recovers
	// downstream harm from on-topic-but-wrong injects better than the threshold
	// alone (docs/experiments.md §B4). Opt-in: a fast local judge (~1s) is viable
	// in-flight for harm-prone vaults; a frontier CLI (~3.5s) is best offline. nil
	// disables it (the default — the cosine gate alone).
	Grounder   grounding.Grounder
	GroundTopK int // candidates to ground per recall (default 3 when Grounder set)
}

// defaultMinScore is the injection threshold on the embedding cosine similarity
// (see normalizeRelevance): a memory is injected only if its effective
// (post-decay) cosine is at least this value, and a turn where nothing clears it
// injects nothing at all. A single absolute threshold answers both "*when* to
// inject" (suppress when no memory is confident enough) and "*what* to inject"
// (keep the memories that are).
//
// 0.6 comes from a benchmark against a REAL MuninnDB instance (cmd/msc-bench):
// over a labeled corpus, gating cosine at 0.6 gave perfect inject/suppress
// accuracy with a clean plateau over [0.575, 0.675]; relevant matches land at
// ~0.6–0.85 and unrelated-topic queries at ~0.4–0.5, so 0.6 sits in the gap.
// (Gating the composite `score` instead is hopeless — it cannot separate the
// two at any threshold; that is why normalizeRelevance switches to cosine.)
const defaultMinScore = 0.6

// defaultRecallMode is the MuninnDB recall preset the injector requests. A
// benchmark against a real instance over a labeled SQuAD corpus (cmd/msc-bench)
// compared all four presets: "semantic" (pure high-precision vector search) gave
// the best retrieval (R@1 and MRR), while "deep" (4-hop graph traversal) and
// "recent" (recency-biased) added noise that lowered it. So the injector asks
// for semantic recall and gates on the cosine it returns.
const defaultRecallMode = "semantic"

// Exported defaults New applies for optional config. Callers that preview what
// New *would* do (e.g. the CLI --dry-run output) should reference these instead
// of re-hardcoding the literals, so the preview cannot drift from the behavior.
const (
	DefaultBudget     = 2048              // max approximate tokens injected per request
	DefaultMinScore   = defaultMinScore   // injection cosine gate
	DefaultRecallMode = defaultRecallMode // MuninnDB recall preset
	DefaultGroundTopK = 3                 // candidates grounded per recall when a Grounder is set
)

// maxWhereLeftOffEntries caps how many previous-session items the one-shot
// session-context block lists. Each entry is already ≤200 runes; bounding the
// count keeps this server-controlled, budget-exempt block from growing without
// limit on the first request. 20 entries (~1k tokens) is ample session bootstrap.
const maxWhereLeftOffEntries = 20

// maxGuideRunes caps the free-form guide text MuninnDB returns for a session
// start. The guide is one opaque string prepended outside the per-memory token
// budget, so it is the one injected block whose size the backend alone
// decides; 2000 runes (~500 tokens) matches the bound on where_left_off's
// free-form fallback and leaves the budget for recalled memories.
const maxGuideRunes = 2000

// intentCacheTTL bounds how long a recall result (or a recall miss) may stand in
// for a fresh query. The sidecar writes memories into the same vault
// continuously, including ones that answer a question that recalled nothing
// earlier in the session, so a cached "nothing found" and a cached window both
// go stale against the vault the injector itself keeps changing. A tool-use
// chain resends the same user message seconds apart, so this only has to
// outlast a round trip, not the session.
const intentCacheTTL = 2 * time.Minute

// Injector enriches LLM API requests with recalled memories from MuninnDB.
// It maintains a session-level memory window: recalled memories persist across
// turns with decaying scores, so context from earlier turns fades gradually
// instead of vanishing when the next query's recall results differ.
type Injector struct {
	mcp           *mcpclient.Client
	vault         string
	budget        int
	threshold     float64
	minScore      float64
	recallMode    string
	querySimReuse float64
	autoCalibrate bool
	timeout       time.Duration
	stats         *stats.Stats
	now           func() time.Time // wall clock for the intent-cache TTL (tests substitute it)

	grounder   grounding.Grounder // optional answer-grounding rerank (nil = cosine gate only)
	groundTopK int

	// Online calibration state (guarded by mu): the sidecar samples effective
	// recall scores and periodically retunes minScore to the noise/relevant
	// valley, so the gate self-improves to the deployment's score distribution
	// instead of trusting the fixed default. See CalibrateThreshold.
	calibScores       []float64
	recallsSinceCalib int
	calibrated        bool
	noVectorWarn      sync.Once

	// lastQuery* track the most recent recall query so continuations (the same
	// user message resent with new tool results) reuse the session window
	// instead of firing a redundant recall. lastWasEmpty drives the negative
	// cache (a repeated intent that already recalled nothing skips re-querying).
	// lastQueryAt bounds that reuse: the vault gains memories mid-session, so a
	// verdict older than intentCacheTTL is re-queried rather than trusted.
	// Guarded by mu.
	lastQueryHash   uint64
	lastQueryTokens []string
	hasLastQuery    bool
	lastWasEmpty    bool
	lastQueryAt     time.Time

	// Session-start: call where_left_off and guide once on first enrichment.
	sessionOnce sync.Once
	sessionCtx  string // one-shot session-start context (where_left_off + guide); eagerly cleared when first read during enrichment

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
		cfg.Budget = DefaultBudget
	}
	if cfg.Threshold <= 0 {
		// The recall floor filters MuninnDB's *composite* score (recency/graph-
		// inflated), a different axis from the cosine the gate uses. Keep it below
		// the gate's calibration floor (calibMinThreshold): otherwise, on a
		// low-cosine vault where auto-calibration lowers MinScore toward 0.10, this
		// server-side pre-filter would silently drop high-cosine-but-low-composite
		// memories the calibrated gate would have injected. The cosine gate
		// (MinScore) does the real suppression; this just avoids returning obvious
		// nothing. (Verified: at composite-threshold 0.4 a cosine-0.45 memory was
		// withheld that 0.05 returned — see docs/experiments.md.)
		cfg.Threshold = 0.05
	}
	if cfg.Threshold > calibMinThreshold {
		slog.Warn("inject: recall floor exceeds the gate calibration floor; calibration below it cannot inject (server pre-filter caps it)",
			"recall_floor", cfg.Threshold, "calib_floor", calibMinThreshold)
	}
	// NaN fails both comparisons, so the test is written as a positive range
	// check: a NaN gate would silently disable the threshold downstream.
	if !(cfg.MinScore > 0 && cfg.MinScore <= 1) {
		cfg.MinScore = defaultMinScore
	}
	if cfg.RecallMode == "" {
		cfg.RecallMode = defaultRecallMode
	}
	if cfg.QuerySimReuse <= 0 || cfg.QuerySimReuse > 1 {
		cfg.QuerySimReuse = 1.0 // exact-match reuse only by default
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 200 * time.Millisecond
	}
	if cfg.Grounder != nil && cfg.GroundTopK <= 0 {
		cfg.GroundTopK = DefaultGroundTopK
	}

	return &Injector{
		mcp:            mcpclient.New(cfg.MCPURL, cfg.Token, cfg.Timeout),
		vault:          cfg.Vault,
		budget:         cfg.Budget,
		threshold:      cfg.Threshold,
		minScore:       cfg.MinScore,
		recallMode:     cfg.RecallMode,
		querySimReuse:  cfg.QuerySimReuse,
		autoCalibrate:  cfg.AutoCalibrate,
		timeout:        cfg.Timeout,
		stats:          cfg.Stats,
		now:            time.Now,
		grounder:       cfg.Grounder,
		groundTopK:     cfg.GroundTopK,
		recentMemories: make(map[string]trackedMemory),
	}
}

// Enrich parses a request body, recalls relevant memories, and injects them
// as system-level context. Returns the enriched body and estimated injected
// token count. Always returns a nil error — all failures are handled
// gracefully by returning the original body unchanged.
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
		// where_left_off is bounded at the source by entry count (parseWhereLeftOff,
		// maxWhereLeftOffEntries) and the guide by length (parseGuide,
		// maxGuideRunes), so the assembled session context is well-formed and
		// bounded without truncating the string (which would cut the closing
		// marker).
		//
		// Defense in depth: scrub secrets/PII before injecting, the same as the
		// per-memory recall block (formatContextBlock). where_left_off and guide are
		// recalled memory content too — a secret stored by another client (or before
		// write-side redaction existed) must not be re-transmitted to the provider.
		inj.mu.Lock()
		inj.sessionCtx = redact.Secrets(sb.String())
		inj.mu.Unlock()
	})

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body, 0, nil // not JSON, pass through
	}

	format := apiformat.DetectFormat(doc)
	if format == "" {
		if slog.Default().Enabled(ctx, slog.LevelDebug) {
			keys := make([]string, 0, len(doc))
			for k := range doc {
				keys = append(keys, k)
			}
			sort.Strings(keys) // map order is per-run random; keep the log replayable
			slog.DebugContext(ctx, "inject: unknown request format, skipping", "keys", keys)
		}
		return body, 0, nil // unknown format, pass through
	}

	// Query with the LATEST user turn alone. A benchmark (cmd/msc-bench
	// -query-transform) showed that folding prior unrelated turns into the query
	// roughly halves retrieval (R@1 0.55→0.21) — the embedding pools all tokens,
	// so distractor context dilutes the signal regardless of order (latest-first
	// did not recover it). Earlier turns are not concatenated; the session window
	// already carries continuity across turns. Fall back to the recent-context
	// extractor only when no single user turn is found (some formats).
	query := apiformat.StripSystemReminders(apiformat.ExtractUserQuery(doc, format))
	if query == "" {
		query = apiformat.StripSystemReminders(apiformat.ExtractRecentContext(doc, format, 3))
	}
	if query == "" {
		slog.Debug("inject: no user query found", "format", format)
		return body, 0, nil // no message to search with
	}

	// Scrub direct identifiers before the query leaves the process. Everything
	// downstream of here (the embed call to MuninnDB, the intent hash, the
	// similarity tokens) is derived from this string, so the same
	// redaction the write path applies covers the read path too. The
	// embedding never benefits from the digits of an email, a card, or a
	// phone number, so the recall cost is nil and the query no longer carries
	// those values to the memory backend. Redacting before the hash also
	// keeps the same-intent cache from treating two spellings of one
	// identifier as different intents.
	query = redact.Secrets(query)

	slog.Debug("inject: recalling", "format", format, "query_len", len(query))

	// 2000 runes balances recall quality against MCP call overhead; longer
	// queries provide diminishing returns for semantic search.
	query = apiformat.TruncateQuery(query, 2000)

	// Decide *whether to ask* MuninnDB. In a tool-use chain the agent resends the
	// same user message with new tool results every round; the user's intent
	// hasn't changed, so re-recalling is wasted latency on the request hot path.
	// When the query is unchanged and the session window still holds memories,
	// reuse the window (continuation) instead of firing a redundant recall.
	qhash := hashQuery(query)
	var curTokens []string
	if inj.querySimReuse < 1 {
		curTokens = wordSet(query)
	}
	inj.mu.Lock()
	// The cached verdict only stands in for a fresh recall while it is young.
	// Past intentCacheTTL the vault may hold memories this query would now
	// match (the sidecar writes to it throughout the session), so ask again.
	cachedFresh := inj.hasLastQuery && inj.now().Sub(inj.lastQueryAt) < intentCacheTTL
	sameIntent := cachedFresh && (qhash == inj.lastQueryHash ||
		(inj.querySimReuse < 1 && len(inj.lastQueryTokens) > 0 &&
			jaccard(curTokens, inj.lastQueryTokens) >= inj.querySimReuse))
	windowEmpty := len(inj.recentMemories) == 0
	negCached := sameIntent && windowEmpty && inj.lastWasEmpty
	inj.mu.Unlock()

	minScore := inj.currentMinScore() // may have been retuned by auto-calibration

	var merged []memory
	switch {
	case sameIntent && !windowEmpty:
		// Continuation of the same intent with memories on hand: reuse the
		// window instead of re-querying (the recall-trigger / "when to ask").
		slog.Debug("inject: same intent, reusing session window (recall skipped)")
		if inj.stats != nil {
			inj.stats.RecallsSkipped.Add(1)
		}
		merged = selectForInjection(inj.snapshotWindow(), minScore)
	case negCached:
		// Negative cache: this intent already recalled nothing useful and the
		// window is empty — skip the redundant recall and inject nothing.
		slog.Debug("inject: same intent previously empty (negative cache), recall skipped")
		if inj.stats != nil {
			inj.stats.RecallsSkipped.Add(1)
		}
		merged = nil
	default:
		// Recall from MuninnDB with timeout.
		recallCtx, cancel := context.WithTimeout(ctx, inj.timeout)
		defer cancel()

		memories, err := inj.recall(recallCtx, query)
		if inj.stats != nil {
			inj.stats.Recalls.Add(1)
		}
		if err != nil {
			slog.Warn("inject: recall failed, passing through", "vault", inj.vault, "err", err)
			if inj.stats != nil {
				inj.stats.InjectionErrors.Add(1)
			}
			return body, 0, nil // graceful fallback
		}
		slog.Debug("inject: recall returned", "count", len(memories))

		inj.observeCalibration(memories) // self-tune the gate to this vault's scores
		merged = selectForInjection(inj.mergeMemories(memories), minScore)
		// Optional answer-grounding rerank: drop gated candidates the judge says
		// don't answer the query (the cross-encoder precision step, §B4). Only on
		// fresh recalls; groundMemories also evicts rejections from the session
		// window, so the same-intent reuse path above holds only vetted memories.
		if inj.grounder != nil && len(merged) > 0 {
			merged = inj.groundMemories(ctx, query, merged)
		}

		inj.mu.Lock()
		inj.lastQueryHash = qhash
		inj.lastQueryTokens = curTokens
		inj.hasLastQuery = true
		inj.lastWasEmpty = len(inj.recentMemories) == 0
		inj.lastQueryAt = inj.now()
		inj.mu.Unlock()
	}

	// Read and clear session context under the lock so only one concurrent
	// enrichment injects it. Clearing eagerly is safe because InjectContext
	// failures are extremely rare (format is already validated) and the
	// context is ephemeral (available in MuninnDB for future recall).
	inj.mu.Lock()
	sessCtx := inj.sessionCtx
	inj.sessionCtx = ""
	inj.mu.Unlock()

	if len(merged) == 0 && sessCtx == "" {
		// The gate chose to inject nothing this turn (no memory cleared the
		// threshold) — the deliberate "when not to inject" outcome.
		if inj.stats != nil {
			inj.stats.Suppressed.Add(1)
		}
		return body, 0, nil
	}

	// Format context block within token budget.
	block, tokens, droppedByBudget := formatContextBlock(merged, inj.budget)
	if droppedByBudget > 0 {
		// The gate passed more memories than the budget fits, so the lowest-scored
		// were silently dropped. Surface it: a large-memory vault may need a higher
		// --inject-budget to avoid losing answer-bearing context.
		slog.Debug("inject: budget truncated gated memories", "dropped", droppedByBudget, "kept", len(merged)-droppedByBudget, "budget", inj.budget)
		if inj.stats != nil {
			inj.stats.BudgetTruncated.Add(int64(droppedByBudget))
		}
	}

	// Prepend session context (where_left_off + guide) on first enrichment.
	if sessCtx != "" {
		block = sessCtx + "\n" + block
		tokens += len(sessCtx) / charPerToken
	}

	block = strings.TrimSpace(block)
	if block == "" {
		return body, 0, nil
	}

	// Inject into the document.
	enriched, err := InjectContext(doc, format, block)
	if err != nil {
		slog.Warn("inject context failed after format validation", "vault", inj.vault, "format", format, "err", err)
		if inj.stats != nil {
			inj.stats.InjectionErrors.Add(1)
		}
		return body, 0, nil
	}

	// Update stats.
	if inj.stats != nil {
		inj.stats.Injections.Add(1)
		inj.stats.InjectedTokens.Add(int64(tokens))
	}

	return enriched, tokens, nil
}

// currentMinScore returns the live injection threshold under the lock (it may be
// retuned by online calibration between turns).
func (inj *Injector) currentMinScore() float64 {
	inj.mu.Lock()
	defer inj.mu.Unlock()
	return inj.minScore
}

// hashQuery returns an FNV-1a hash of the recall query, used to detect that a
// request is a continuation of the same user intent (so recall can be skipped).
func hashQuery(query string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(query))
	return h.Sum64()
}

// groundMemories applies the answer-grounding rerank to the gated set: the top
// groundTopK by score are dropped unless the judge says they answer the query;
// lower-ranked candidates are kept untouched (they are rarely the high-cosine
// wrong-passage case grounding targets, and judging them all would add latency).
// The grounder fails open, so an unavailable judge degrades to the cosine gate.
func (inj *Injector) groundMemories(ctx context.Context, query string, mems []memory) []memory {
	n := inj.groundTopK
	if n <= 0 || n > len(mems) {
		n = len(mems)
	}
	passages := make([]string, n)
	for i := 0; i < n; i++ {
		passages[i] = mems[i].Content
	}
	mask := inj.grounder.Relevant(ctx, query, passages) // one listwise call
	kept := make([]memory, 0, len(mems))
	var droppedIDs []string
	for i, m := range mems {
		if i < n && i < len(mask) && !mask[i] {
			droppedIDs = append(droppedIDs, m.ID)
			continue
		}
		kept = append(kept, m)
	}
	// Evict judge-rejected memories from the session window too. Continuations
	// of the same intent reuse the window without re-grounding (Enrich's
	// same-intent path), so leaving a rejected memory there would re-inject it
	// on every subsequent round of the tool-use loop. A later *different* intent
	// re-recalls and re-grounds it against the new query, so eviction loses nothing.
	if len(droppedIDs) > 0 {
		inj.mu.Lock()
		for _, id := range droppedIDs {
			delete(inj.recentMemories, id)
		}
		inj.mu.Unlock()
	}
	dropped := len(droppedIDs)
	slog.Debug("inject: grounding rerank", "judged", n, "dropped", dropped, "kept", len(kept), "judge", inj.grounder.Label())
	if inj.stats != nil {
		inj.stats.GroundingRuns.Add(1)
		if dropped > 0 {
			inj.stats.GroundDropped.Add(int64(dropped))
		}
	}
	return kept
}
