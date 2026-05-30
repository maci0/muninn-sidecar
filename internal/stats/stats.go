// Package stats provides lightweight session-level statistics for msc.
// All counters are safe for concurrent use from multiple goroutines.
package stats

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Stats tracks session-level counters for proxy and store activity.
type Stats struct {
	Captured    atomic.Int64 // total exchanges entering Store() (includes those later dropped, deduped, skipped, or failed to flush)
	Dropped     atomic.Int64 // exchanges dropped (queue full)
	Deduped     atomic.Int64 // exchanges skipped (duplicate concept)
	Skipped     atomic.Int64 // exchanges skipped (empty content or noise patterns)
	Flushed     atomic.Int64 // exchanges delivered to MuninnDB
	FlushErrors atomic.Int64 // delivery failures (after retries)
	TokensIn    atomic.Int64 // total input/prompt tokens
	TokensOut   atomic.Int64 // total output/completion tokens
	CacheWrite  atomic.Int64 // Anthropic cache_creation_input_tokens
	CacheRead   atomic.Int64 // Anthropic cache_read_input_tokens

	Injections      atomic.Int64 // requests enriched with recalled memories
	InjectedTokens  atomic.Int64 // approximate tokens injected across all enrichments
	InjectionErrors atomic.Int64 // enrichment failures (inject fell back to original body)
	Suppressed      atomic.Int64 // requests where the gate chose to inject nothing (no memory cleared the threshold)
	Recalls         atomic.Int64 // recall calls actually fired to MuninnDB
	RecallsSkipped  atomic.Int64 // recalls avoided by reusing the session window (unchanged-query continuations)

	models sync.Map // model name → *atomic.Int64
}

// RecordModel increments the usage count for a model.
func (s *Stats) RecordModel(model string) {
	if model == "" {
		return
	}
	v, _ := s.models.LoadOrStore(model, &atomic.Int64{})
	v.(*atomic.Int64).Add(1)
}

// Models returns a snapshot of model usage counts, sorted by count descending.
func (s *Stats) Models() []ModelCount {
	var out []ModelCount
	s.models.Range(func(key, value any) bool {
		out = append(out, ModelCount{
			Name:  key.(string),
			Count: value.(*atomic.Int64).Load(),
		})
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].Count > out[j].Count
	})
	return out
}

// ModelCount holds a model name and its usage count.
type ModelCount struct {
	Name  string
	Count int64
}

// Summary returns a human-readable session summary. Returns empty string
// if no exchanges were captured or dropped and no injections happened.
func (s *Stats) Summary() string {
	captured := s.Captured.Load()
	dropped := s.Dropped.Load()
	deduped := s.Deduped.Load()
	skipped := s.Skipped.Load()
	flushed := s.Flushed.Load()
	errors := s.FlushErrors.Load()

	injections := s.Injections.Load()
	injTokens := s.InjectedTokens.Load()

	if captured == 0 && dropped == 0 && injections == 0 {
		return ""
	}

	var sb strings.Builder

	// Line 1: exchange counts.
	sb.WriteString(fmt.Sprintf("session: %d saved", flushed))
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
	// Individual atomic loads are non-atomic as a group, so a concurrent flush
	// can make the arithmetic transiently negative; clamp before display.
	if queued := max(captured-dropped-flushed-deduped-skipped-errors, 0); queued > 0 {
		sb.WriteString(fmt.Sprintf(" (%d queued)", queued))
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
		}
	}

	// Line 4: model breakdown (only if we tracked any).
	models := s.Models()
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

// formatCount formats a number with K/M suffixes for readability.
func formatCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 10_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
