package stats

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
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
	if !strings.Contains(got, "2 WebSocket/upgrade stream") {
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

func TestSummaryWithUpstreamErrors(t *testing.T) {
	s := &Stats{}
	s.Captured.Store(10)
	s.Flushed.Store(10)
	s.UpstreamErrors.Store(4)

	got := s.Summary()
	if !strings.Contains(got, "4 upstream errors") {
		t.Fatalf("expected '4 upstream errors' in summary: %q", got)
	}
}

func TestSummaryUpstreamErrorsOnly(t *testing.T) {
	// A session whose only signal is upstream errors must still report them
	// rather than returning an empty summary (the count is otherwise swallowed).
	s := &Stats{}
	s.UpstreamErrors.Store(3)

	got := s.Summary()
	if !strings.Contains(got, "3 upstream errors") {
		t.Fatalf("expected '3 upstream errors' in summary: %q", got)
	}
}

func TestSummaryNoUpstreamErrorLineWhenZero(t *testing.T) {
	s := &Stats{}
	s.Captured.Store(5)
	s.Flushed.Store(5)

	if strings.Contains(s.Summary(), "upstream errors") {
		t.Fatalf("upstream errors line should be absent when none occurred: %q", s.Summary())
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

// The model name arrives from the request body, so the distinct-name set is the
// client's to grow. Recording must stop at the cap and account for the rest.
func TestRecordModelDistinctNamesBounded(t *testing.T) {
	s := &Stats{}
	for i := range maxTrackedModels * 3 {
		s.RecordModel(fmt.Sprintf("model-%d", i))
	}
	if got := len(s.Models()); got != maxTrackedModels {
		t.Errorf("tracked %d distinct models, want the cap %d", got, maxTrackedModels)
	}
	if got, want := s.ModelsDropped(), int64(maxTrackedModels*2); got != want {
		t.Errorf("ModelsDropped = %d, want %d", got, want)
	}
	// A name already inside the cap still counts after it is full.
	s.RecordModel("model-0")
	if got := s.ModelsDropped(); got != maxTrackedModels*2 {
		t.Errorf("recording a tracked name changed ModelsDropped to %d", got)
	}
	for _, m := range s.Models() {
		if m.Name == "model-0" && m.Count != 2 {
			t.Errorf("model-0 count = %d, want 2", m.Count)
		}
	}
}

// The summary must not present a capped breakdown as if it were the whole one.
func TestSummaryReportsUntrackedModels(t *testing.T) {
	s := &Stats{}
	s.Captured.Store(1)
	s.Flushed.Store(1)
	s.RecordModel("claude-3-opus")
	for i := range maxTrackedModels * 2 {
		s.RecordModel(fmt.Sprintf("other-%d", i))
	}

	got := s.Summary()
	if !strings.Contains(got, "claude-3-opus (1)") {
		t.Errorf("tracked model missing from summary: %q", got)
	}
	if !strings.Contains(got, "other (") {
		t.Errorf("summary omits the untracked remainder: %q", got)
	}
}

// One model name is client-supplied text copied verbatim from the request body;
// it must not be retained or printed at unbounded length.
func TestRecordModelTruncatesLongName(t *testing.T) {
	s := &Stats{}
	s.RecordModel(strings.Repeat("m", maxModelNameLen*10))

	models := s.Models()
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if got := utf8.RuneCountInString(models[0].Name); got != maxModelNameLen {
		t.Errorf("model name length = %d runes, want %d", got, maxModelNameLen)
	}
}

// The cap counts bytes, not runes: a byte cut splits a multi-byte character and
// leaves invalid UTF-8 in the tracked name and in the summary that prints it.
func TestRecordModelCapKeepsNonASCIIWhole(t *testing.T) {
	s := &Stats{}
	s.Requests.Add(1) // the summary stays empty until something happened
	s.RecordModel(strings.Repeat("模", maxModelNameLen*2))

	models := s.Models()
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if !utf8.ValidString(models[0].Name) {
		t.Errorf("tracked model name is not valid UTF-8: %q", models[0].Name)
	}
	if !strings.Contains(s.Summary(), models[0].Name) {
		t.Errorf("summary does not carry the tracked name: %q", s.Summary())
	}
}

// A byte-count cap alone would leave a broken UTF-8 sequence at the end of a
// long non-ASCII model name, and that broken string is both the map key and
// what the session summary prints — a replacement character on the one line an
// operator reads to identify the model in use. The clip must back off to a rune
// boundary, at the cost of being a few bytes under the cap.
func TestRecordModelTruncatesOnRuneBoundary(t *testing.T) {
	s := &Stats{}
	// Every rune is 3 bytes, so the cap lands mid-rune for any length here.
	name := strings.Repeat("模", maxModelNameLen)
	s.RecordModel(name + strings.Repeat("型", 40))
	s.Requests.Add(1) // Summary reports nothing without session activity

	models := s.Models()
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	got := models[0].Name
	if len(got) > maxModelNameLen {
		t.Errorf("model name length = %d, over the %d-byte cap", len(got), maxModelNameLen)
	}
	if len(got) == 0 {
		t.Fatal("clipping a multibyte name produced nothing")
	}
	if !utf8.ValidString(got) {
		t.Errorf("clip left invalid UTF-8: %q", got)
	}
	if !strings.HasPrefix(name, got) {
		t.Errorf("clip is not a prefix of the name: %q", got)
	}
	if !strings.Contains(s.Summary(), got) {
		t.Errorf("summary %q does not carry the clipped name %q", s.Summary(), got)
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

func TestObserveLatency(t *testing.T) {
	s := &Stats{}
	s.ObserveLatency(100)
	s.ObserveLatency(300)
	s.ObserveLatency(200)

	n, mean, max := s.Latency()
	if n != 3 || mean != 200 || max != 300 {
		t.Fatalf("Latency() = (%d, %d, %d), want (3, 200, 300)", n, mean, max)
	}
}

func TestObserveLatencyIgnoresNegative(t *testing.T) {
	// A clock that ran backwards must not pull the session mean below zero.
	s := &Stats{}
	s.ObserveLatency(500)
	s.ObserveLatency(-1)

	n, mean, max := s.Latency()
	if n != 1 || mean != 500 || max != 500 {
		t.Fatalf("Latency() = (%d, %d, %d), want (1, 500, 500)", n, mean, max)
	}
}

func TestLatencyEmpty(t *testing.T) {
	s := &Stats{}
	if n, mean, max := s.Latency(); n != 0 || mean != 0 || max != 0 {
		t.Fatalf("Latency() = (%d, %d, %d), want zeros", n, mean, max)
	}
}

func TestSummaryWithLatency(t *testing.T) {
	s := &Stats{}
	s.Captured.Store(2)
	s.Flushed.Store(2)
	s.ObserveLatency(1200)
	s.ObserveLatency(900)

	got := s.Summary()
	if !strings.Contains(got, "latency: 1.1s avg, 1.2s slowest (2 completed)") {
		t.Fatalf("expected latency line in summary: %q", got)
	}
}

func TestSummaryNoLatencyLineWhenUnobserved(t *testing.T) {
	s := &Stats{}
	s.Captured.Store(3)
	s.Flushed.Store(3)

	if strings.Contains(s.Summary(), "latency:") {
		t.Fatalf("latency line should be absent when nothing was observed: %q", s.Summary())
	}
}

func TestSummaryProxyErrorsOnly(t *testing.T) {
	// A session that only ever failed to reach the upstream (no captures) must
	// still report it rather than printing an empty summary.
	s := &Stats{}
	s.ProxyErrors.Store(2)

	got := s.Summary()
	if got == "" {
		t.Fatal("expected non-empty summary for proxy errors alone")
	}
	if !strings.Contains(got, "2 proxy errors") {
		t.Fatalf("expected '2 proxy errors' in summary: %q", got)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		ms   int64
		want string
	}{
		{0, "0ms"},
		{812, "812ms"},
		{1000, "1.0s"},
		{45000, "45.0s"},
		{60000, "1.0min"},
		{243000, "4.0min"},
	}

	for _, tt := range tests {
		if got := formatDuration(tt.ms); got != tt.want {
			t.Errorf("formatDuration(%d) = %q, want %q", tt.ms, got, tt.want)
		}
	}
}

func TestSummaryReportsRequestRate(t *testing.T) {
	// Captures alone cannot distinguish a working proxy from one the agent has
	// stopped calling: both show a session that saved nothing and errored
	// nothing. The request count is the other half of the rate.
	s := &Stats{}
	s.Requests.Store(12)
	s.Captured.Store(3)
	s.Flushed.Store(3)

	got := s.Summary()
	if !strings.Contains(got, "12 requests") {
		t.Errorf("summary does not report the request count: %q", got)
	}
	if !strings.Contains(got, "3 saved") {
		t.Errorf("summary lost the saved count: %q", got)
	}
}

func TestSummaryReportsIdleSession(t *testing.T) {
	// Requests but no captures still has to print: that is the session where
	// capture is broken, and silence would hide it.
	s := &Stats{}
	s.Requests.Store(4)

	if got := s.Summary(); !strings.Contains(got, "4 requests") {
		t.Errorf("idle session produced no summary line: %q", got)
	}
}
