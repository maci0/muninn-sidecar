// Package inject provides automatic memory retrieval and injection into
// LLM API requests. It recalls relevant memories from MuninnDB and injects
// them as system-level context before forwarding requests upstream.
package inject

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/maci0/muninn-sidecar/internal/apiformat"
	"github.com/maci0/muninn-sidecar/internal/grounding"
	"github.com/maci0/muninn-sidecar/internal/mcpclient"
	"github.com/maci0/muninn-sidecar/internal/stats"
)

// charPerToken is the character-to-token approximation used throughout the inject
// package. 4 chars ≈ 1 token is a standard heuristic for English prose; it is
// intentionally imprecise since injection stays well within model context limits.
const charPerToken = 4

// memory represents a recalled memory from MuninnDB.
//
// MuninnDB returns two relevance numbers: Score is a composite that folds in
// recency and graph traversal (it can exceed 1.0 and, critically, does not
// separate relevant from irrelevant — a benchmark against a real instance found
// it injects on irrelevant queries just as readily as relevant ones). VectorScore
// is the raw embedding cosine similarity, which separates cleanly. The injector
// gates and ranks on VectorScore (falling back to Score only when the field is
// absent); see normalizeRelevance and cmd/msc-bench.
type memory struct {
	ID          string  `json:"id"`
	Concept     string  `json:"concept"`
	Content     string  `json:"content"`
	Score       float64 `json:"score"`
	VectorScore float64 `json:"vector_score"`
	// CreatedAt is the memory's creation timestamp (RFC3339 UTC, e.g.
	// "2026-05-30T14:32:02Z"). Because it is zero-padded UTC, lexical string
	// comparison is chronological — used to prefer the fresher of two
	// near-duplicate memories so an updated fact wins over the stale one it
	// supersedes (anti-staleness; see selectForInjection).
	CreatedAt string `json:"created_at"`
	// State is the MuninnDB lifecycle state (planning|active|paused|blocked|
	// completed|cancelled|archived, or "" if unset). Trust is the reliability
	// level (verified|inferred|external|untrusted). Explicitly-dead or untrusted
	// memories are excluded from injection (see injectable).
	State string `json:"state"`
	Trust string `json:"trust"`
}

// injectable reports whether a recalled memory is fit to inject. It excludes
// memories MuninnDB has marked as explicitly dead — `archived` (retired) or
// `cancelled` (abandoned) — and `untrusted` ones (flagged unreliable). Surfacing
// any of these as current context misleads the agent. `completed` is kept: a
// finished task's decisions/facts remain relevant. Empty/unrecognized values are
// kept, so vaults that don't populate these fields see no change.
func injectable(m memory) bool {
	switch m.State {
	case "archived", "cancelled":
		return false
	}
	return m.Trust != "untrusted"
}

// normalizeRelevance rewrites each memory's Score to the gating/ranking signal:
// the embedding cosine (VectorScore) when present, else the original composite
// Score as a fallback. After this, the whole downstream pipeline (decay,
// ordering, threshold, the displayed relevance) operates on cosine similarity.
// Returns whether any memory carried a cosine — when none do, the gate is
// operating on the recency/graph-inflated composite, which is far less reliable.
func normalizeRelevance(mems []memory) (anyVector bool) {
	for i := range mems {
		if mems[i].VectorScore > 0 {
			mems[i].Score = mems[i].VectorScore
			anyVector = true
		}
	}
	return anyVector
}

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

// decayFactor is multiplied against a memory's score for each turn it is
// not re-recalled. This creates a gradual fade-out rather than abrupt removal.
const decayFactor = 0.7

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

// dupTokenOverlap is the Jaccard similarity (over lowercased word sets) above
// which two memories are considered near-duplicates. Only the higher-scored of
// a duplicate pair is injected, so redundant memories don't waste the budget or
// dilute the injected context. 0.8 catches re-phrasings and supersets without
// collapsing genuinely distinct memories that merely share vocabulary.
const dupTokenOverlap = 0.8

// decayFloor is the minimum effective score before a memory is evicted from
// the session window. With decayFactor=0.7 and a starting score of 0.85,
// a memory survives ~4 turns without being re-recalled before falling below 0.2.
const decayFloor = 0.2

// decayTable holds precomputed decay multipliers for ages 0–9 turns.
// decayTable[n] = 0.7^n. Memories are evicted at age ~4–5, so the table
// covers all practical cases without calling math.Pow.
var decayTable = [10]float64{
	1.0,
	0.7,
	0.49,
	0.343,
	0.2401,
	0.16807,
	0.117649,
	0.0823543,
	0.05764801,
	0.040353607,
}

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
	// Guarded by mu.
	lastQueryHash   uint64
	lastQueryTokens []string
	hasLastQuery    bool
	lastWasEmpty    bool

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
		cfg.Budget = 2048
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
	if cfg.MinScore <= 0 || cfg.MinScore > 1 {
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
		cfg.GroundTopK = 3
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
		if slog.Default().Enabled(ctx, slog.LevelDebug) {
			keys := make([]string, 0, len(doc))
			for k := range doc {
				keys = append(keys, k)
			}
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
	sameIntent := inj.hasLastQuery && (qhash == inj.lastQueryHash ||
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
		// fresh recalls — the window holds already-vetted memories.
		if inj.grounder != nil && len(merged) > 0 {
			merged = inj.groundMemories(ctx, query, merged)
		}

		inj.mu.Lock()
		inj.lastQueryHash = qhash
		inj.lastQueryTokens = curTokens
		inj.hasLastQuery = true
		inj.lastWasEmpty = len(inj.recentMemories) == 0
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

// decayedScore returns the effective score of a memory that was last seen
// `age` turns ago, applying the per-turn decay factor.
func decayedScore(score float64, age int) float64 {
	if age < len(decayTable) {
		return score * decayTable[age]
	}
	return score * math.Pow(decayFactor, float64(age))
}

// currentMinScore returns the live injection threshold under the lock (it may be
// retuned by online calibration between turns).
func (inj *Injector) currentMinScore() float64 {
	inj.mu.Lock()
	defer inj.mu.Unlock()
	return inj.minScore
}

// Online-calibration tuning constants.
const (
	calibCap         = 400 // rolling sample window
	calibMinSamples  = 40  // first calibration once this many scores seen
	calibRefreshEach = 30  // recalibrate every N recalls thereafter (track drift)
)

// observeCalibration feeds the effective scores of a fresh recall into the
// rolling sample and retunes minScore toward the noise/relevant valley once
// enough data is seen, then periodically to track drift. No-op unless
// auto-calibration is enabled.
func (inj *Injector) observeCalibration(mems []memory) {
	if !inj.autoCalibrate || len(mems) == 0 {
		return
	}
	inj.mu.Lock()
	defer inj.mu.Unlock()

	for _, m := range mems {
		inj.calibScores = append(inj.calibScores, m.Score)
	}
	if len(inj.calibScores) > calibCap {
		inj.calibScores = inj.calibScores[len(inj.calibScores)-calibCap:]
	}
	inj.recallsSinceCalib++

	due := (!inj.calibrated && len(inj.calibScores) >= calibMinSamples) ||
		(inj.calibrated && inj.recallsSinceCalib >= calibRefreshEach)
	if !due {
		return
	}
	newT := CalibrateThreshold(inj.calibScores)
	if newT != inj.minScore {
		slog.Info("inject: auto-calibrated injection threshold",
			"old", inj.minScore, "new", newT, "samples", len(inj.calibScores))
	}
	inj.minScore = newT
	inj.calibrated = true
	inj.recallsSinceCalib = 0
}

// hashQuery returns an FNV-1a hash of the recall query, used to detect that a
// request is a continuation of the same user intent (so recall can be skipped).
func hashQuery(query string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(query))
	return h.Sum64()
}

// snapshotWindow returns the current session window decayed at the current turn
// WITHOUT advancing the turn counter or evicting — used on continuations, where
// no new intent has arrived so memories should neither age nor be re-recalled.
func (inj *Injector) snapshotWindow() []memory {
	inj.mu.Lock()
	defer inj.mu.Unlock()

	cur := inj.turn
	out := make([]memory, 0, len(inj.recentMemories))
	for _, tm := range inj.recentMemories {
		m := tm.memory
		m.Score = decayedScore(m.Score, cur-tm.lastSeen)
		out = append(out, m)
	}
	if len(out) > 1 {
		sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	}
	return out
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
		if decayedScore(tm.Score, turnsAgo) < decayFloor {
			delete(inj.recentMemories, id)
			slog.Debug("inject: evicted stale memory", "id", id, "age", turnsAgo)
		}
	}

	// Build sorted output from the merged window.
	merged := make([]memory, 0, len(inj.recentMemories))
	for _, tm := range inj.recentMemories {
		turnsAgo := currentTurn - tm.lastSeen
		m := tm.memory
		m.Score = decayedScore(m.Score, turnsAgo)
		merged = append(merged, m)
	}
	if len(merged) > 1 {
		sort.Slice(merged, func(i, j int) bool {
			return merged[i].Score > merged[j].Score
		})
	}

	return merged
}

// Calibration bounds: a wide clamp so the gate can adapt to low-cosine
// deployments (e.g. short query vs long memory, where the relevant cluster sits
// near 0.2) as well as high-cosine ones, while still rejecting degenerate ends.
const (
	calibMinThreshold = 0.10
	calibMaxThreshold = 0.90
	// calibMinSeparation is the minimum gap between the two cluster means (in
	// score units) for the sample to count as bimodal. Below it the distribution
	// is effectively unimodal — no real noise/relevant split — so we keep the
	// default prior instead of trusting an arbitrary Otsu cut.
	calibMinSeparation = 0.08
)

// CalibrateThreshold derives an injection threshold from a sample of recall
// scores instead of relying on the hand-picked default. Recall scores tend to be
// bimodal — a noise cluster and a relevant cluster — so it finds the valley
// between them with Otsu's method (the split maximizing between-class variance).
//
// Crucially, the valley is adopted ONLY when the two clusters are meaningfully
// separated (>= calibMinSeparation); on a unimodal sample it returns
// defaultMinScore. This lets the gate self-tune to whatever the deployment's
// embedding/query shape produces — including low-cosine vaults where a fixed 0.6
// would suppress everything — without latching onto noise. Returns defaultMinScore
// for a too-small sample.
func CalibrateThreshold(scores []float64) float64 {
	if len(scores) < 20 {
		return defaultMinScore
	}
	const bins = 50
	hist := make([]int, bins)
	for _, s := range scores {
		s = math.Max(0, math.Min(1, s))
		b := int(s * bins)
		if b >= bins {
			b = bins - 1
		}
		hist[b]++
	}
	total := len(scores)
	var sumAll float64
	for b, c := range hist {
		sumAll += float64(b) * float64(c)
	}

	var wB int
	var sumB float64
	bestVar, bestT, bestSep := -1.0, defaultMinScore, 0.0
	for t := 0; t < bins; t++ {
		wB += hist[t]
		if wB == 0 {
			continue
		}
		wF := total - wB
		if wF == 0 {
			break
		}
		sumB += float64(t) * float64(hist[t])
		mB := sumB / float64(wB)
		mF := (sumAll - sumB) / float64(wF)
		between := float64(wB) * float64(wF) * (mB - mF) * (mB - mF)
		if between > bestVar {
			bestVar = between
			bestT = (float64(t) + 1) / float64(bins) // upper edge of the noise bin
			bestSep = (mF - mB) / float64(bins)      // cluster-mean gap in score units
		}
	}
	if bestSep < calibMinSeparation {
		return defaultMinScore // not confidently bimodal — keep the prior
	}
	return math.Max(calibMinThreshold, math.Min(calibMaxThreshold, bestT))
}

// selectForInjection is the full inject decision for a turn. It expects input
// pre-sorted by effective score (descending), as mergeMemories returns, and
// applies two filters:
//
//  1. Absolute threshold (minScore): keep only memories whose effective score is
//     at least minScore. Because this drops every candidate when none is
//     confident enough, it decides *when* to inject (an empty result suppresses
//     the turn) and *what* to inject in one step. The empirical method study
//     (eval_study.go) found this single-threshold rule matches a separate
//     relative cutoff + gate while being simpler and wasting less budget.
//
//  2. Near-duplicate removal: a memory is dropped if it duplicates an
//     already-kept memory — either by identical normalized concept or by high
//     word-set overlap of content. This keeps the injected block from spending
//     budget on redundant restatements of the same fact.
//
//     For same-concept duplicates (one concept = one fact), the *fresher* memory
//     wins rather than the higher-cosine one: an updated fact ("migrated to
//     Postgres") supersedes the stale restatement it duplicates ("we use MySQL"),
//     even if the stale one scored marginally higher. This is the anti-staleness
//     behavior a long-lived vault needs; recall ranks by similarity, not by
//     which statement is currently true. Cross-concept content-overlap dups keep
//     the higher-cosine one (they may be genuinely distinct facts).
//
// minScore <= 0 disables the threshold (every recalled memory is eligible).
func selectForInjection(merged []memory, minScore float64) []memory {
	if len(merged) == 0 {
		return merged
	}

	kept := make([]memory, 0, len(merged))
	keptTokens := make([][]string, 0, len(merged))
	conceptIdx := make(map[string]int, len(merged))

	for _, m := range merged {
		if minScore > 0 && m.Score < minScore {
			break // sorted descending — nothing after this clears the threshold
		}

		if !injectable(m) {
			slog.Debug("inject: skipped unfit memory", "id", m.ID, "state", m.State, "trust", m.Trust)
			continue
		}

		concept := normalizeConcept(m.Concept)
		if concept != "" {
			if i, ok := conceptIdx[concept]; ok {
				// Same fact already kept: keep whichever is fresher.
				if m.CreatedAt > kept[i].CreatedAt {
					slog.Debug("inject: replaced stale same-concept memory with fresher", "concept", m.Concept, "old_created", kept[i].CreatedAt, "new_created", m.CreatedAt)
					kept[i] = m
					keptTokens[i] = wordSet(m.Content)
				} else {
					slog.Debug("inject: skipped duplicate-concept memory (not fresher)", "id", m.ID, "concept", m.Concept)
				}
				continue
			}
		}

		tokens := wordSet(m.Content)
		if isNearDuplicate(tokens, keptTokens) {
			slog.Debug("inject: skipped near-duplicate memory", "id", m.ID, "concept", m.Concept)
			continue
		}

		kept = append(kept, m)
		keptTokens = append(keptTokens, tokens)
		if concept != "" {
			conceptIdx[concept] = len(kept) - 1
		}
	}

	return kept
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
	dropped := 0
	for i, m := range mems {
		if i < n && i < len(mask) && !mask[i] {
			dropped++
			continue
		}
		kept = append(kept, m)
	}
	slog.Debug("inject: grounding rerank", "judged", n, "dropped", dropped, "kept", len(kept), "judge", inj.grounder.Label())
	if inj.stats != nil {
		inj.stats.GroundingRuns.Add(1)
		if dropped > 0 {
			inj.stats.GroundDropped.Add(int64(dropped))
		}
	}
	return kept
}

// normalizeConcept lowercases and trims a concept for duplicate detection so
// "Auth Pattern" and "auth pattern " collapse to the same key.
func normalizeConcept(c string) string {
	return strings.ToLower(strings.TrimSpace(c))
}

// wordSet returns the set of distinct lowercased whitespace-delimited words in
// s, used to estimate content overlap without embeddings.
func wordSet(s string) []string {
	fields := strings.Fields(strings.ToLower(s))
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}

// isNearDuplicate reports whether tokens overlaps any previously-kept token set
// with Jaccard similarity at or above dupTokenOverlap.
func isNearDuplicate(tokens []string, kept [][]string) bool {
	if len(tokens) == 0 {
		return false
	}
	for _, k := range kept {
		if jaccard(tokens, k) >= dupTokenOverlap {
			return true
		}
	}
	return false
}

// jaccard returns the Jaccard similarity (|A∩B| / |A∪B|) of two word sets.
// Both inputs are expected to contain no duplicates (see wordSet).
func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := make(map[string]struct{}, len(a))
	for _, w := range a {
		set[w] = struct{}{}
	}
	inter := 0
	for _, w := range b {
		if _, ok := set[w]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

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

// parseMCPTextContent extracts the text from the first text-typed content block
// in a JSON-RPC response. Both parseGuide and parseWhereLeftOff share this structure.
func parseMCPTextContent(body []byte) string {
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
	for _, c := range rpcResp.Result.Content {
		if c.Type == "text" && c.Text != "" {
			return c.Text
		}
	}
	return ""
}

// parseGuide extracts the guide text from the JSON-RPC response.
func parseGuide(body []byte) string {
	text := strings.TrimSpace(parseMCPTextContent(body))
	if text == "" {
		return ""
	}
	return apiformat.GlobalGuideOpen + "\n" + text + "\n" + apiformat.GlobalGuideClose
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
		// If not JSON, use the raw text if it's meaningful.
		text := strings.TrimSpace(raw)
		if text != "" && text != "[]" && text != "null" {
			return apiformat.SessionContextOpen + "\nPrevious session context:\n" + apiformat.TruncateText(text, 2000) + "\n" + apiformat.SessionContextClose
		}
		return ""
	}

	if len(wloResult.Memories) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString(apiformat.SessionContextOpen + "\nPrevious session context:\n")
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
// The MCP response wraps the tool result in result.content[].text.
// JSON-RPC protocol errors are handled by mcpclient.Client.Call before
// this function is called, so this function only receives success responses.
func parseRecallResponse(body []byte) ([]memory, error) {
	var rpcResp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}

	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("parse JSON-RPC response: %w", err)
	}

	// The recall tool returns its result as JSON text inside content[n].text.
	// The loop skips non-text items and returns results from the first parseable text block.
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
				return nil, fmt.Errorf("parse recall result (struct: %v, array: %w)", err, err2)
			}
			return direct, nil
		}

		if len(recallResult.Memories) > 0 {
			return recallResult.Memories, nil
		}
		return recallResult.Results, nil
	}

	return nil, nil
}
