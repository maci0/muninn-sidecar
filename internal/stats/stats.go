// Package stats provides lightweight session-level statistics for msc.
// All counters are safe for concurrent use from multiple goroutines.
package stats

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/maci0/muninn-sidecar/internal/strhash"
)

// Stats tracks session-level counters for proxy and store activity.
type Stats struct {
	// Requests counts every request the proxy accepted, captured or not. It is
	// the rate numerator for the whole session: Captured only counts exchanges
	// that reached the store, so a proxy that is answering but no longer
	// capturing (paths changed, store disabled) and a proxy the agent has
	// stopped calling look identical without it.
	Requests    atomic.Int64 // requests accepted by the proxy (status endpoint excluded)
	Captured    atomic.Int64 // total exchanges entering Store() (includes those later dropped, deduped, skipped, or failed to flush)
	Dropped     atomic.Int64 // exchanges dropped (queue full, byte budget exhausted, or after drain)
	Deduped     atomic.Int64 // exchanges skipped (duplicate concept)
	Skipped     atomic.Int64 // exchanges skipped (empty content or noise patterns)
	Flushed     atomic.Int64 // exchanges delivered to MuninnDB
	FlushErrors atomic.Int64 // delivery failures (after retries)
	TokensIn    atomic.Int64 // total input/prompt tokens
	TokensOut   atomic.Int64 // total output/completion tokens
	CacheWrite  atomic.Int64 // Anthropic cache_creation_input_tokens
	CacheRead   atomic.Int64 // Anthropic cache_read_input_tokens

	UpstreamErrors atomic.Int64 // captured responses with a 4xx/5xx status from the upstream LLM API
	ProxyErrors    atomic.Int64 // requests the proxy failed itself (dial/TLS/transport error, agent got a 502)

	// Uncapturable counts responses that matched a captured path but whose
	// bytes this proxy cannot read: gRPC protobuf, a non-gzip content
	// encoding, a protocol upgrade. They forward to the agent untouched and
	// never reach the store, so without this counter a session that is
	// upstream on such a response looks identical to one that is working.
	Uncapturable atomic.Int64

	Injections      atomic.Int64 // requests enriched with recalled memories
	InjectedTokens  atomic.Int64 // approximate tokens injected across all enrichments
	InjectionErrors atomic.Int64 // enrichment failures (inject fell back to original body)
	Suppressed      atomic.Int64 // requests where the gate chose to inject nothing (no memory cleared the threshold)
	Recalls         atomic.Int64 // recall calls actually fired to MuninnDB
	RecallsSkipped  atomic.Int64 // recalls avoided by reusing the session window (unchanged-query continuations)
	GroundingRuns   atomic.Int64 // turns the answer-grounding rerank ran (one listwise judge call each)
	GroundDropped   atomic.Int64 // candidates the grounding rerank dropped (judged not to answer the query)
	BudgetTruncated atomic.Int64 // gated memories dropped because they exceeded the injection token budget
	Upgraded        atomic.Int64 // protocol-upgrade (e.g. WebSocket) streams spliced through MITM; a codex Responses stream is still decoded and captured on the tap

	models sync.Map // model hash → *modelEntry

	// modelNames counts the distinct names currently in models, so RecordModel
	// can stop growing the map at maxTrackedModels without a second lookup.
	modelNames atomic.Int64
	// modelsDropped counts calls that named a model beyond the tracked set, so
	// the summary reports an honest total instead of a silently truncated one.
	modelsDropped atomic.Int64

	// Response-time accounting, in milliseconds, for the two legs an operator
	// has to tell apart when a turn slows down: the whole exchange, and the
	// MuninnDB recall that runs in front of it. Kept as three separate counters
	// rather than a histogram: a session is a few dozen turns, so a mean and a
	// max answer "was the agent slow, and was it the recall" without the
	// storage a distribution would cost.
	latency     latencyCounters // completed exchanges
	recallTimes latencyCounters // recall calls to MuninnDB
}

// latencyCounters holds one leg's count, total, and maximum. Safe for
// concurrent use from multiple goroutines.
type latencyCounters struct {
	n   atomic.Int64
	sum atomic.Int64
	max atomic.Int64
}

// observe records one sample in milliseconds. Negative values are ignored: a
// clock that ran backwards would otherwise pull the mean below zero.
func (l *latencyCounters) observe(ms int64) {
	if ms < 0 {
		return
	}
	l.n.Add(1)
	l.sum.Add(ms)
	for {
		cur := l.max.Load()
		if ms <= cur || l.max.CompareAndSwap(cur, ms) {
			return
		}
	}
}

// read returns the number of observed samples, the mean in milliseconds, and
// the slowest in milliseconds. All three are 0 when nothing has been observed.
func (l *latencyCounters) read() (n, meanMs, maxMs int64) {
	n = l.n.Load()
	if n == 0 {
		return 0, 0, 0
	}
	return n, l.sum.Load() / n, l.max.Load()
}

// ObserveLatency records one completed exchange's response time in
// milliseconds. Negative values are ignored: a clock that ran backwards
// would otherwise pull the session mean below zero.
func (s *Stats) ObserveLatency(ms int64) { s.latency.observe(ms) }

// Latency returns the number of observed exchanges, the mean response time in
// milliseconds, and the slowest response in milliseconds. All three are 0 when
// nothing has been observed.
func (s *Stats) Latency() (n, meanMs, maxMs int64) { return s.latency.read() }

// ObserveRecallLatency records one recall call's round trip to MuninnDB in
// milliseconds, so a slow store can be told apart from a slow upstream. The
// recall runs on the request hot path ahead of every captured turn, so its cost
// is inside the exchange latency the summary already reports; without this the
// only evidence of a degraded store is that turns got slower.
func (s *Stats) ObserveRecallLatency(ms int64) { s.recallTimes.observe(ms) }

// RecallLatency returns the number of observed recalls, the mean round trip in
// milliseconds, and the slowest in milliseconds. All three are 0 when no recall
// has been made.
func (s *Stats) RecallLatency() (n, meanMs, maxMs int64) { return s.recallTimes.read() }

// Snapshot is a point-in-time copy of the session counters, for the status
// endpoint and anything else that needs the numbers outside Summary's
// human-readable rendering.
type Snapshot struct {
	Requests      int64 `json:"requests"`
	Captured      int64 `json:"captured"`
	Saved         int64 `json:"saved"`
	Dropped       int64 `json:"dropped"`
	SaveErrors    int64 `json:"save_errors"`
	UpstreamError int64 `json:"upstream_errors"`
	ProxyErrors   int64 `json:"proxy_errors"`
	Uncapturable  int64 `json:"uncapturable_responses"`
	Injections    int64 `json:"injections"`
	InjectErrors  int64 `json:"injection_errors"`
	Recalls       int64 `json:"recalls"`
	LatencyN      int64 `json:"latency_samples"`
	LatencyMeanMs int64 `json:"latency_mean_ms"`
	LatencyMaxMs  int64 `json:"latency_max_ms"`

	RecallLatencyN      int64 `json:"recall_latency_samples"`
	RecallLatencyMeanMs int64 `json:"recall_latency_mean_ms"`
	RecallLatencyMaxMs  int64 `json:"recall_latency_max_ms"`
}

// Snapshot returns the current counter values.
func (s *Stats) Snapshot() Snapshot {
	n, mean, max := s.Latency()
	recallN, recallMean, recallMax := s.RecallLatency()
	return Snapshot{
		Requests:            s.Requests.Load(),
		Captured:            s.Captured.Load(),
		Saved:               s.Flushed.Load(),
		Dropped:             s.Dropped.Load(),
		SaveErrors:          s.FlushErrors.Load(),
		UpstreamError:       s.UpstreamErrors.Load(),
		ProxyErrors:         s.ProxyErrors.Load(),
		Uncapturable:        s.Uncapturable.Load(),
		Injections:          s.Injections.Load(),
		InjectErrors:        s.InjectionErrors.Load(),
		Recalls:             s.Recalls.Load(),
		LatencyN:            n,
		LatencyMeanMs:       mean,
		LatencyMaxMs:        max,
		RecallLatencyN:      recallN,
		RecallLatencyMeanMs: recallMean,
		RecallLatencyMaxMs:  recallMax,
	}
}

// maxTrackedModels caps the distinct model names a session tracks. The name
// comes from the request body, so its cardinality is the client's: an agent (or
// a loop hitting the proxy) that varies the model field per request would grow
// this map without bound for the life of the process. A real session uses a
// handful of models; the rest are counted in ModelsDropped and reported as
// "other".
const maxTrackedModels = 16

// maxModelNameLen caps the model name as it is retained and printed, counted
// in bytes. A name is client-supplied text copied verbatim from the request
// body, so without this a single multi-megabyte "model" string is held for the
// session and printed into the summary. clipBytes keeps the cut on a character
// boundary, because the clipped name lands in the summary, where a split
// character would leave invalid UTF-8 in it.
//
// The cap is a display cap, not an identity. Clipping before the map insert made
// the truncated string the key, so two distinct long names sharing their first
// 64 bytes counted as one model and the summary printed a name that was neither.
// The key is a hash of the full name and only the stored name is clipped.
const maxModelNameLen = 64

// modelEntry is one tracked model: the name as it is reported, and its count.
type modelEntry struct {
	name  string
	count atomic.Int64
}

// RecordModel increments the usage count for a model. Names past
// maxTrackedModels are counted in ModelsDropped rather than tracked
// individually.
func (s *Stats) RecordModel(model string) {
	if model == "" {
		return
	}
	key := strhash.FNV1a(model)
	if v, loaded := s.models.Load(key); loaded {
		v.(*modelEntry).count.Add(1)
		return
	}
	if s.modelNames.Load() >= maxTrackedModels {
		s.modelsDropped.Add(1)
		return
	}
	v, loaded := s.models.LoadOrStore(key, &modelEntry{name: clipBytes(model, maxModelNameLen)})
	if !loaded {
		s.modelNames.Add(1)
	}
	v.(*modelEntry).count.Add(1)
}

// clipBytes truncates s to at most max bytes without splitting a multi-byte
// character. A byte count alone would leave a replacement character at the end
// of a long non-ASCII model name, and that broken string is what the session
// summary prints — the one place a name is shown to a human. The same guard the
// SSE text accumulator and the context budget packer apply to their own byte
// caps.
func clipBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 {
		// A last rune decoding to RuneError at width 1 is either the tail of a
		// sequence the cap cut in half or a stray invalid byte; both go, and
		// only those: a valid character ending inside the cap ends the trim.
		if r, size := utf8.DecodeLastRuneInString(s); r == utf8.RuneError && size <= 1 {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}

// ModelsDropped returns the number of requests whose model was not tracked
// individually because the tracked set was full.
func (s *Stats) ModelsDropped() int64 { return s.modelsDropped.Load() }

// Models returns a snapshot of model usage counts, sorted by count descending.
func (s *Stats) Models() []ModelCount {
	var out []ModelCount
	s.models.Range(func(key, value any) bool {
		entry := value.(*modelEntry)
		out = append(out, ModelCount{
			Name:  entry.name,
			Count: entry.count.Load(),
		})
		return true
	})
	// sync.Map iterates in an unspecified order, so tie the count sort on name:
	// equally-used models must keep a fixed position in the summary line.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ModelCount holds a model name and its usage count.
type ModelCount struct {
	Name  string
	Count int64
}

// Summary returns a human-readable session summary. Returns empty string only
// when nothing at all happened: no requests, no captures or drops, no
// injections, no upgraded connections, and no upstream or proxy errors.
func (s *Stats) Summary() string {
	requests := s.Requests.Load()
	captured := s.Captured.Load()
	dropped := s.Dropped.Load()
	deduped := s.Deduped.Load()
	skipped := s.Skipped.Load()
	flushed := s.Flushed.Load()
	errors := s.FlushErrors.Load()

	injections := s.Injections.Load()
	injTokens := s.InjectedTokens.Load()

	upgraded := s.Upgraded.Load()
	upstreamErrors := s.UpstreamErrors.Load()
	proxyErrors := s.ProxyErrors.Load()

	if requests == 0 && captured == 0 && dropped == 0 && injections == 0 && upgraded == 0 &&
		upstreamErrors == 0 && proxyErrors == 0 {
		return ""
	}

	var sb strings.Builder

	// Line 1: request and exchange counts. Requests come first because they are
	// the denominator the rest of the line is read against: "12 requests, 3
	// saved" says whether captures are keeping up, and a session whose agent
	// stopped calling shows a falling request count even when nothing errored.
	sb.WriteString(fmt.Sprintf("session: %d requests, %d saved", requests, flushed))
	if deduped > 0 {
		sb.WriteString(fmt.Sprintf(", %d deduped", deduped))
	}
	if skipped > 0 {
		sb.WriteString(fmt.Sprintf(", %d skipped", skipped))
	}
	if dropped > 0 {
		sb.WriteString(fmt.Sprintf(", %d dropped", dropped))
	}
	if errors > 0 {
		sb.WriteString(fmt.Sprintf(", %d save errors", errors))
	}
	if upstreamErrors > 0 {
		sb.WriteString(fmt.Sprintf(", %d upstream errors", upstreamErrors))
	}
	if proxyErrors > 0 {
		sb.WriteString(fmt.Sprintf(", %d proxy errors", proxyErrors))
	}
	// A turn the proxy could not read is a turn that was never a memory, and it
	// is invisible everywhere else: it neither reaches the queued arithmetic
	// above nor the latency sample below. Naming it here is the difference
	// between "0 saved" and "0 saved, 12 responses this proxy cannot decode".
	if uncapturable := s.Uncapturable.Load(); uncapturable > 0 {
		sb.WriteString(fmt.Sprintf(", %d not captured (undecodable response)", uncapturable))
	}
	// Individual atomic loads are non-atomic as a group, so a concurrent flush
	// can make the arithmetic transiently negative; clamp before display.
	if queued := max(captured-dropped-flushed-deduped-skipped-errors, 0); queued > 0 {
		sb.WriteString(fmt.Sprintf(" (%d queued)", queued))
	}

	// Response time for the turns that completed. Without this a session that
	// got slower shows no sign of it: the exchange and token counts look the
	// same on a fast and a degraded upstream.
	if n, meanMs, maxMs := s.Latency(); n > 0 {
		sb.WriteString(fmt.Sprintf("\nlatency: %s avg, %s slowest (%d completed)",
			formatDuration(meanMs), formatDuration(maxMs), n))
	}

	// Line 2: token totals (only if we saw any).
	tokIn := s.TokensIn.Load()
	tokOut := s.TokensOut.Load()
	cacheW := s.CacheWrite.Load()
	cacheR := s.CacheRead.Load()
	if tokIn > 0 || tokOut > 0 {
		sb.WriteString(fmt.Sprintf("\ntokens: %s in / %s out",
			formatCount(tokIn), formatCount(tokOut)))
		if cacheW > 0 || cacheR > 0 {
			sb.WriteString(fmt.Sprintf(" (cache: %s write, %s read)",
				formatCount(cacheW), formatCount(cacheR)))
		}
	}

	// Line 3: injection stats (only if any injections happened).
	injErrors := s.InjectionErrors.Load()
	suppressed := s.Suppressed.Load()
	recalls := s.Recalls.Load()
	recallsSkipped := s.RecallsSkipped.Load()
	if injections > 0 || injErrors > 0 || suppressed > 0 {
		sb.WriteString(fmt.Sprintf("\ninject: %d injected, %d suppressed, ~%s tokens",
			injections, suppressed, formatCount(injTokens)))
		if injErrors > 0 {
			sb.WriteString(fmt.Sprintf(", %d errors", injErrors))
		}
		if recalls > 0 || recallsSkipped > 0 {
			sb.WriteString(fmt.Sprintf("\nrecall: %d queried, %d reused (window)", recalls, recallsSkipped))
			// The recall runs ahead of every turn, so its round trip is inside
			// the latency above. Naming it separately is what tells a slow
			// MuninnDB apart from a slow upstream.
			if n, meanMs, maxMs := s.RecallLatency(); n > 0 {
				sb.WriteString(fmt.Sprintf("; %s avg, %s slowest", formatDuration(meanMs), formatDuration(maxMs)))
			}
		}
		if gruns := s.GroundingRuns.Load(); gruns > 0 {
			sb.WriteString(fmt.Sprintf("\ngrounding: %d turns judged, %d candidates dropped", gruns, s.GroundDropped.Load()))
		}
		if bt := s.BudgetTruncated.Load(); bt > 0 {
			sb.WriteString(fmt.Sprintf("\nbudget: %d memories truncated (raise --inject-budget)", bt))
		}
	}

	// MITM-spliced protocol upgrades (WebSocket). codex's Responses-over-WebSocket
	// is decoded and captured (counted in "saved" above); other upgrades pass
	// through without capture.
	if upgraded > 0 {
		sb.WriteString(fmt.Sprintf("\nmitm: %d WebSocket/upgrade stream(s) spliced", upgraded))
	}

	// Line 4: model breakdown (only if we tracked any).
	models := s.Models()
	if dropped := s.ModelsDropped(); dropped > 0 {
		// Name more than the tracked set allows: say so rather than print a
		// breakdown that silently omits the rest.
		if len(models) == 0 {
			sb.WriteString(fmt.Sprintf("\nmodels: %d untracked (more than %d distinct)", dropped, maxTrackedModels))
		} else {
			models = append(models, ModelCount{Name: "other", Count: dropped})
		}
	}
	if len(models) > 0 {
		sb.WriteString("\nmodels: ")
		parts := make([]string, 0, len(models))
		for _, m := range models {
			parts = append(parts, fmt.Sprintf("%s (%d)", m.Name, m.Count))
		}
		sb.WriteString(strings.Join(parts, ", "))
	}

	return sb.String()
}

// formatCount formats a number with K/M suffixes for readability. The unit is
// chosen from the value as scaled and rounded, not from the raw count: 999,999
// scales to 1000.0K, which reads as more than the 1.0M printed for 1,000,000,
// so a count that rounds up into the next unit is printed in that unit. The
// rounding test is in integers because comparing the scaled float against the
// rounding boundary would leave the cutoff to depend on how the tie rounds.
func formatCount(n int64) string {
	const (
		perK = 1_000
		perM = 1_000_000
		// minAbbreviated is the smallest count worth abbreviating at all: past
		// it the raw digits stop being quicker to read.
		minAbbreviated = 10 * perK
		// minMillions is the smallest count that rounds to 1000.0K, 999.95K, and
		// is therefore printed in millions.
		minMillions = perM - 50
	)
	switch {
	case n >= minMillions:
		return fmt.Sprintf("%.1fM", float64(n)/perM)
	case n >= minAbbreviated:
		return fmt.Sprintf("%.1fK", float64(n)/perK)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// formatDuration formats a millisecond duration for the summary line: whole
// milliseconds below a second, then seconds with one decimal, then minutes
// (LLM turns of several minutes are normal for large contexts).
func formatDuration(ms int64) string {
	switch {
	case ms < 1_000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1_000)
	default:
		return fmt.Sprintf("%.1fmin", float64(ms)/60_000)
	}
}
