package store

import (
	"encoding/json"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/maci0/muninn-sidecar/internal/stats"
)

// FuzzFormatAndDedup exercises the untrusted ingestion pipeline: a captured
// request/response pair (raw intercepted bytes) flows through system-reminder
// stripping, user/assistant extraction, secret redaction, noise filtering,
// concept building, FNV dedup, and tag construction. Invariants: never panic;
// a produced memory has valid-UTF-8 concept/content and at least the three
// base tags. Redaction is toggled to cover both paths.
func FuzzFormatAndDedup(f *testing.F) {
	f.Add(
		`{"messages":[{"role":"user","content":[{"type":"text","text":"how to sort in go"}]}]}`,
		`{"content":[{"type":"text","text":"use sort.Slice"}],"usage":{"output_tokens":5}}`,
		"claude", 200, true)
	f.Add(
		`{"input":"my key is sk-ant-0000000000000000000000000000000000000000000000000000"}`,
		`{"output":[{"type":"message","content":[{"text":"ok"}]}]}`,
		"gemini", 500, true)
	f.Add(`not json`, ``, "", 0, false)
	f.Add(`{"messages":[]}`, `{}`, "codex", 404, false)

	st := &stats.Stats{}
	s := &MuninnStore{stats: st}

	f.Fuzz(func(t *testing.T, req, resp, agent string, status int, redact bool) {
		s.SetRedaction(redact)

		ex := &CapturedExchange{
			Timestamp:  time.Unix(0, 0).UTC(),
			Agent:      agent,
			Method:     "POST",
			Path:       "/v1/messages",
			StatusCode: status,
			ReqBody:    json.RawMessage(req),
			RespBody:   json.RawMessage(resp),
		}

		// Fresh ring per iteration: dedup state must not leak across fuzz inputs.
		var ring [dedupRingSize]map[uint64]struct{}
		ringIdx := 0

		fm := s.formatAndDedup(ex, &ring, &ringIdx)
		if fm == nil {
			return // dropped as empty/noise/dup — a valid outcome.
		}

		// Concept and content derive from JSON string fields and TruncateText,
		// both of which must preserve UTF-8 validity (corruption here would
		// poison every stored memory and recall query).
		if !utf8.ValidString(fm.concept) {
			t.Fatalf("concept is not valid UTF-8: %q", fm.concept)
		}
		if !utf8.ValidString(fm.content) {
			t.Fatalf("content is not valid UTF-8: %q", fm.content)
		}
		// buildTags always emits sidecar + agent + status tags.
		if len(fm.tags) < 3 {
			t.Fatalf("expected >= 3 base tags, got %v", fm.tags)
		}

		// A non-nil memory must not be silently empty: at least one side carried
		// content, so the concept is never blank.
		if fm.concept == "" {
			t.Fatalf("non-nil memory has empty concept (req=%q resp=%q)", req, resp)
		}
	})
}
