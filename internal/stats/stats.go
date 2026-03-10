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
	Captured       atomic.Int64 // exchanges successfully queued
	Dropped        atomic.Int64 // exchanges dropped (queue full)
	Deduped        atomic.Int64 // exchanges skipped (duplicate concept)
	Flushed        atomic.Int64 // exchanges delivered to MuninnDB
	FlushErrors    atomic.Int64 // delivery failures (after retries)
	TokensIn       atomic.Int64 // total input/prompt tokens
	TokensOut      atomic.Int64 // total output/completion tokens
	CacheWrite     atomic.Int64 // Anthropic cache_creation_input_tokens
	CacheRead      atomic.Int64 // Anthropic cache_read_input_tokens
	Injections     atomic.Int64 // requests enriched with recalled memories
	InjectedTokens atomic.Int64 // approximate tokens injected across all enrichments

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
// if no exchanges were captured or dropped (nothing interesting happened).
func (s *Stats) Summary() string {
	captured := s.Captured.Load()
	dropped := s.Dropped.Load()
	deduped := s.Deduped.Load()
	flushed := s.Flushed.Load()
	errors := s.FlushErrors.Load()

	if captured == 0 && dropped == 0 {
		return ""
	}

	var sb strings.Builder

	// Line 1: exchange counts.
	sb.WriteString(fmt.Sprintf("session: %d captured", flushed))
	if deduped > 0 {
		sb.WriteString(fmt.Sprintf(", %d deduped", deduped))
	}
	if dropped > 0 {
		sb.WriteString(fmt.Sprintf(", %d dropped", dropped))
	}
	if errors > 0 {
		sb.WriteString(fmt.Sprintf(", %d errors", errors))
	}
	if captured != flushed+deduped {
		// Some are still in queue (shouldn't happen after drain, but be safe).
		sb.WriteString(fmt.Sprintf(" (%d queued)", captured-flushed-deduped))
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
	injections := s.Injections.Load()
	injTokens := s.InjectedTokens.Load()
	if injections > 0 {
		sb.WriteString(fmt.Sprintf("\ninject: %d enriched, %s tokens injected",
			injections, formatCount(injTokens)))
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
