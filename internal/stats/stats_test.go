package stats

import (
	"strings"
	"testing"
)

func TestSummaryEmpty(t *testing.T) {
	s := &Stats{}
	if got := s.Summary(); got != "" {
		t.Fatalf("expected empty summary for no activity, got %q", got)
	}
}

func TestSummaryUpgradedOnly(t *testing.T) {
	// A session that only spliced WebSocket upgrades (e.g. codex) captured
	// nothing, but must still surface the uncaptured-stream notice.
	s := &Stats{}
	s.Upgraded.Store(2)
	got := s.Summary()
	if got == "" {
		t.Fatal("expected non-empty summary when upgrades occurred")
	}
	if !strings.Contains(got, "2 upgraded stream") || !strings.Contains(got, "not captured") {
		t.Fatalf("expected upgraded-stream notice in summary: %q", got)
	}
}

func TestSummaryBasic(t *testing.T) {
	s := &Stats{}
	s.Captured.Store(5)
	s.Flushed.Store(5)
	s.TokensIn.Store(1000)
	s.TokensOut.Store(500)

	got := s.Summary()
	if !strings.Contains(got, "5 saved") {
		t.Fatalf("expected '5 saved' in summary: %q", got)
	}
	if !strings.Contains(got, "1000 in") {
		t.Fatalf("expected '1000 in' in summary: %q", got)
	}
	if !strings.Contains(got, "500 out") {
		t.Fatalf("expected '500 out' in summary: %q", got)
	}
}

func TestSummaryWithDropsAndErrors(t *testing.T) {
	s := &Stats{}
	s.Captured.Store(10)
	s.Flushed.Store(8)
	s.Dropped.Store(3)
	s.FlushErrors.Store(2)

	got := s.Summary()
	if !strings.Contains(got, "3 dropped") {
		t.Fatalf("expected '3 dropped' in summary: %q", got)
	}
	if !strings.Contains(got, "2 save errors") {
		t.Fatalf("expected '2 save errors' in summary: %q", got)
	}
}

func TestSummaryWithCacheTokens(t *testing.T) {
	s := &Stats{}
	s.Captured.Store(1)
	s.Flushed.Store(1)
	s.TokensIn.Store(1000)
	s.TokensOut.Store(500)
	s.CacheWrite.Store(200)
	s.CacheRead.Store(300)

	got := s.Summary()
	if !strings.Contains(got, "cache:") {
		t.Fatalf("expected cache info in summary: %q", got)
	}
	if !strings.Contains(got, "200 write") {
		t.Fatalf("expected '200 write' in summary: %q", got)
	}
	if !strings.Contains(got, "300 read") {
		t.Fatalf("expected '300 read' in summary: %q", got)
	}
}

func TestSummaryWithModels(t *testing.T) {
	s := &Stats{}
	s.Captured.Store(3)
	s.Flushed.Store(3)

	s.RecordModel("claude-3-opus")
	s.RecordModel("claude-3-opus")
	s.RecordModel("claude-3-haiku")

	got := s.Summary()
	if !strings.Contains(got, "claude-3-opus (2)") {
		t.Fatalf("expected 'claude-3-opus (2)' in summary: %q", got)
	}
	if !strings.Contains(got, "claude-3-haiku (1)") {
		t.Fatalf("expected 'claude-3-haiku (1)' in summary: %q", got)
	}
}

func TestRecordModelEmpty(t *testing.T) {
	s := &Stats{}
	s.RecordModel("") // should be a no-op
	models := s.Models()
	if len(models) != 0 {
		t.Fatalf("expected no models recorded for empty string, got %d", len(models))
	}
}

func TestModelsSortedByCount(t *testing.T) {
	s := &Stats{}
	s.RecordModel("a")
	s.RecordModel("b")
	s.RecordModel("b")
	s.RecordModel("c")
	s.RecordModel("c")
	s.RecordModel("c")

	models := s.Models()
	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(models))
	}
	if models[0].Name != "c" || models[0].Count != 3 {
		t.Fatalf("expected c(3) first, got %s(%d)", models[0].Name, models[0].Count)
	}
	if models[1].Name != "b" || models[1].Count != 2 {
		t.Fatalf("expected b(2) second, got %s(%d)", models[1].Name, models[1].Count)
	}
	if models[2].Name != "a" || models[2].Count != 1 {
		t.Fatalf("expected a(1) third, got %s(%d)", models[2].Name, models[2].Count)
	}
}

func TestSummaryWithInjections(t *testing.T) {
	s := &Stats{}
	s.Captured.Store(5)
	s.Flushed.Store(5)
	s.Injections.Store(8)
	s.InjectedTokens.Store(6400)

	s.Suppressed.Store(2)
	s.Recalls.Store(7)
	s.RecallsSkipped.Store(3)

	got := s.Summary()
	if !strings.Contains(got, "8 injected") {
		t.Fatalf("expected '8 injected' in summary: %q", got)
	}
	if !strings.Contains(got, "2 suppressed") {
		t.Fatalf("expected '2 suppressed' in summary: %q", got)
	}
	if !strings.Contains(got, "~6400 tokens") {
		t.Fatalf("expected '~6400 tokens' in summary: %q", got)
	}
	if !strings.Contains(got, "7 queried, 3 reused") {
		t.Fatalf("expected recall line in summary: %q", got)
	}
}

func TestSummaryWithGrounding(t *testing.T) {
	s := &Stats{}
	s.Captured.Store(1)
	s.Flushed.Store(1)
	s.Injections.Store(4)
	s.GroundingRuns.Store(4)
	s.GroundDropped.Store(6)

	got := s.Summary()
	if !strings.Contains(got, "4 turns judged, 6 candidates dropped") {
		t.Fatalf("expected grounding line in summary: %q", got)
	}
}

func TestSummaryWithBudgetTruncation(t *testing.T) {
	s := &Stats{}
	s.Captured.Store(1)
	s.Flushed.Store(1)
	s.Injections.Store(3)
	s.BudgetTruncated.Store(5)

	got := s.Summary()
	if !strings.Contains(got, "5 memories truncated") {
		t.Fatalf("expected budget truncation line in summary: %q", got)
	}
}

func TestSummaryNoGroundingLineWhenUnused(t *testing.T) {
	s := &Stats{}
	s.Captured.Store(1)
	s.Flushed.Store(1)
	s.Injections.Store(2)

	if strings.Contains(s.Summary(), "grounding:") {
		t.Fatalf("grounding line should be absent when grounding never ran: %q", s.Summary())
	}
}

func TestSummaryInjectionTokensFormatted(t *testing.T) {
	s := &Stats{}
	s.Captured.Store(1)
	s.Flushed.Store(1)
	s.Injections.Store(3)
	s.InjectedTokens.Store(15000)

	got := s.Summary()
	if !strings.Contains(got, "~15.0K tokens") {
		t.Fatalf("expected '~15.0K tokens' in summary: %q", got)
	}
}

func TestSummaryWithDeduped(t *testing.T) {
	s := &Stats{}
	s.Captured.Store(10)
	s.Deduped.Store(3)
	s.Flushed.Store(7)

	got := s.Summary()
	if !strings.Contains(got, "7 saved") {
		t.Fatalf("expected '7 saved' in summary: %q", got)
	}
	if !strings.Contains(got, "3 deduped") {
		t.Fatalf("expected '3 deduped' in summary: %q", got)
	}
	// Should not show "queued" since captured == flushed + deduped.
	if strings.Contains(got, "queued") {
		t.Fatalf("unexpected 'queued' in summary: %q", got)
	}
}

func TestFormatCount(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{9999, "9999"},
		{10000, "10.0K"},
		{50000, "50.0K"},
		{1000000, "1.0M"},
		{2500000, "2.5M"},
	}

	for _, tt := range tests {
		if got := formatCount(tt.n); got != tt.want {
			t.Errorf("formatCount(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
