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

func TestSummaryBasic(t *testing.T) {
	s := &Stats{}
	s.Captured.Store(5)
	s.Flushed.Store(5)
	s.TokensIn.Store(1000)
	s.TokensOut.Store(500)

	got := s.Summary()
	if !strings.Contains(got, "5 captured") {
		t.Fatalf("expected '5 captured' in summary: %q", got)
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
	if !strings.Contains(got, "2 errors") {
		t.Fatalf("expected '2 errors' in summary: %q", got)
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
